package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// check 子命令：Go 项目提交前自检（移植自 go-check.ps1，只依赖 go + git）。
//
// 检查项：
//  1. gofmt 格式（CRLF 安全，不会误报；装了 gofumpt 则附加检查）
//  2. go vet
//  3. go build
//  4. go test + 全量覆盖率
//  5. 增量覆盖率（相对基线分支的改动行，自行解析 coverage.out）
//  6. 非测试代码新增行数上限
//  7. golangci-lint（若已安装）
//
// 用法：
//
//	httpbench check
//	httpbench check -base origin/main -min-coverage 68 -min-diff-coverage 50
//	httpbench check -fix    # 只格式化本次改动的文件

var (
	covLineRe  = regexp.MustCompile(`^(.+?):(\d+)\.\d+,(\d+)\.\d+\s+\d+\s+(\d+)$`)
	moduleRe   = regexp.MustCompile(`^\s*module\s+(\S+)`)
	diffFileRe = regexp.MustCompile(`^\+\+\+ b/(.+)$`)
	hunkRe     = regexp.MustCompile(`^@@ -\S+ \+(\d+)(?:,(\d+))? @@`)
	numstatRe  = regexp.MustCompile(`^(\d+)\s`)
)

type checkItem struct{ name, detail string }

type checkReport struct {
	passed, warned, failed []checkItem
}

func (r *checkReport) pass(name, detail string) { r.passed = append(r.passed, checkItem{name, detail}) }
func (r *checkReport) warn(name, detail string) { r.warned = append(r.warned, checkItem{name, detail}) }
func (r *checkReport) fail(name, detail string) { r.failed = append(r.failed, checkItem{name, detail}) }

func step(msg string) { fmt.Printf("\n==> %s\n", msg) }

func splitLines(out string) []string {
	return strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n")
}

func nonEmptyLines(out string) []string {
	var res []string
	for _, l := range splitLines(out) {
		if l = strings.TrimSpace(l); l != "" {
			res = append(res, l)
		}
	}
	return res
}

func dedup(sorted []string) []string {
	out := make([]string, 0, len(sorted))
	for i, s := range sorted {
		if i == 0 || s != sorted[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// runCmd 在 dir 下执行命令（dir 为空表示当前目录），返回 stdout、stderr 与退出码。
// stdout 与 stderr 分开收集：列表解析类调用只读 stdout，避免 git 的行尾告警等
// stderr 噪声污染结果。命令不存在或无法启动时 code 为 -1。
func runCmd(dir, name string, args ...string) (stdout, stderr string, code int) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return outBuf.String(), errBuf.String(), ee.ExitCode()
		}
		return outBuf.String(), errBuf.String(), -1
	}
	return outBuf.String(), errBuf.String(), 0
}

func printCmdOutput(stdout, stderr string) {
	if stdout != "" {
		fmt.Print(stdout)
	}
	if stderr != "" {
		fmt.Print(stderr)
	}
}

// findTool 优先从 PATH 找工具，找不到再回退到 GOROOT/bin（gofmt 不一定在 PATH）。
func findTool(name string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	if out, _, code := runCmd("", "go", "env", "GOROOT"); code == 0 {
		goroot := strings.TrimSpace(out)
		for _, cand := range []string{filepath.Join(goroot, "bin", name), filepath.Join(goroot, "bin", name+".exe")} {
			if st, err := os.Stat(cand); err == nil && !st.IsDir() {
				return cand
			}
		}
	}
	return ""
}

// findModuleDir 从当前目录向上查找最近的 go.mod 所在目录。
func findModuleDir() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func readModuleName(goModPath string) string {
	data, err := os.ReadFile(goModPath)
	if err != nil {
		return ""
	}
	for _, l := range splitLines(string(data)) {
		if m := moduleRe.FindStringSubmatch(l); m != nil {
			return m[1]
		}
	}
	return ""
}

// formatterNeedsFix 判断文件经格式化工具处理后是否有变化。
// 先把 CRLF 归一成 LF 再对比，避免行尾差异导致误报。
func formatterNeedsFix(tool, file string) bool {
	raw, err := os.ReadFile(file)
	if err != nil {
		return true
	}
	norm := strings.ReplaceAll(string(raw), "\r\n", "\n")
	cmd := exec.Command(tool)
	cmd.Stdin = strings.NewReader(norm)
	out, err := cmd.Output()
	if err != nil {
		return true
	}
	formatted := string(out)
	if !strings.HasSuffix(formatted, "\n") {
		formatted += "\n"
	}
	return norm != formatted
}

// parseDiffAddedLines 从 git diff -U0 输出解析每个文件新增行所在的行号。
func parseDiffAddedLines(diff string) map[string][]int {
	added := map[string][]int{}
	curFile := ""
	newLine := 0
	for _, l := range splitLines(diff) {
		switch {
		case strings.HasPrefix(l, "@@"):
			if m := hunkRe.FindStringSubmatch(l); m != nil {
				newLine, _ = strconv.Atoi(m[1])
			}
		case strings.HasPrefix(l, "+++") || strings.HasPrefix(l, "---"):
			if m := diffFileRe.FindStringSubmatch(l); m != nil {
				curFile = strings.TrimSpace(m[1])
			}
		case strings.HasPrefix(l, "-"):
			// 删除行不占用新文件行号
		case strings.HasPrefix(l, "+"):
			if curFile != "" {
				added[curFile] = append(added[curFile], newLine)
			}
			newLine++
		case strings.HasPrefix(l, " "):
			newLine++ // 上下文行（-U0 下通常没有）
		}
	}
	return added
}

// countLines 统计文件行数（最后一个无换行符的行也计入）。
func countLines(data []byte) int {
	n := bytes.Count(data, []byte("\n"))
	if len(data) > 0 && data[len(data)-1] != '\n' {
		n++
	}
	return n
}

// runCheck 执行提交前自检，返回退出码：0 全部通过，1 存在失败项。
func runCheck(args []string) int {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	base := fs.String("base", "", "基线分支（默认依次探测 origin/main、origin/master、main、master、origin/develop，失败则用 HEAD）")
	minCoverage := fs.Float64("min-coverage", 0, "全量语句覆盖率下限，0 表示不检查")
	minDiffCoverage := fs.Float64("min-diff-coverage", 0, "增量语句覆盖率下限，0 表示不检查")
	maxAddedLines := fs.Int("max-added-lines", 0, "非测试代码新增行数上限，0 表示不检查")
	fix := fs.Bool("fix", false, "自动格式化本次改动的文件（gofmt -w）")
	skipLint := fs.Bool("skip-lint", false, "跳过 golangci-lint")
	includeTests := fs.Bool("include-tests", false, "增量覆盖率统计包含 _test.go")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "用法: httpbench check [flags]   # Go 项目提交前自检")
		fmt.Fprintln(os.Stderr, "")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)

	rep := &checkReport{}

	// ---------- 0. 环境 ----------
	if findTool("go") == "" {
		fmt.Fprintln(os.Stderr, "go not found")
		return 1
	}
	rootOut, _, code := runCmd("", "git", "rev-parse", "--show-toplevel")
	if code != 0 {
		fmt.Fprintln(os.Stderr, "not a git repo")
		return 1
	}
	repoRoot := strings.TrimSpace(rootOut)

	moduleDir := findModuleDir()
	if moduleDir == "" {
		moduleDir = repoRoot
	}
	if _, err := os.Stat(filepath.Join(moduleDir, "go.mod")); err != nil {
		fmt.Fprintf(os.Stderr, "go.mod not found: %s\n", moduleDir)
		return 1
	}
	module := readModuleName(filepath.Join(moduleDir, "go.mod"))

	fmt.Printf("repo  : %s\n", repoRoot)
	fmt.Printf("module: %s (%s)\n", module, moduleDir)

	// ---------- 1. 基线分支 ----------
	if *base == "" {
		for _, c := range []string{"origin/main", "origin/master", "main", "master", "origin/develop"} {
			if _, _, c2 := runCmd(repoRoot, "git", "rev-parse", "--verify", c); c2 == 0 {
				*base = c
				break
			}
		}
	}
	if *base == "" {
		*base = "HEAD"
	}
	fmt.Printf("base  : %s\n", *base)

	// moduleDir 相对 repoRoot 的路径，用于把 git 的仓库相对路径换算成模块相对路径
	moduleRepoRel := "."
	if rel, err := filepath.Rel(repoRoot, moduleDir); err == nil {
		moduleRepoRel = filepath.ToSlash(rel)
	}
	toModuleRel := func(repoRel string) (string, bool) {
		if moduleRepoRel == "." {
			return repoRel, true
		}
		if strings.HasPrefix(repoRel, moduleRepoRel+"/") {
			return strings.TrimPrefix(repoRel, moduleRepoRel+"/"), true
		}
		return "", false
	}

	// ---------- 2. 收集改动文件 ----------
	untrackedOut, _, _ := runCmd(repoRoot, "git", "ls-files", "--others", "--exclude-standard", "--", "*.go")
	untracked := nonEmptyLines(untrackedOut)
	var changed []string
	if *base != "HEAD" {
		diffOut, _, _ := runCmd(repoRoot, "git", "diff", "--name-only", *base, "--", "*.go")
		changed = append(changed, nonEmptyLines(diffOut)...)
		cachedOut, _, _ := runCmd(repoRoot, "git", "diff", "--cached", "--name-only", "--", "*.go")
		changed = append(changed, nonEmptyLines(cachedOut)...)
	}
	changed = append(changed, untracked...)
	sort.Strings(changed)
	changed = dedup(changed)
	fmt.Printf("changed go files: %d\n", len(changed))

	// ---------- 3. 格式检查（CRLF 安全） ----------
	step("format (gofmt / gofumpt)")
	gofmt := findTool("gofmt")
	if len(changed) == 0 {
		fmt.Println("no changed go files, skipped")
		rep.pass("format", "no changes")
	} else if gofmt == "" {
		fmt.Println("gofmt not found, skipped")
		rep.warn("format", "gofmt not found")
	} else if *fix {
		for _, f := range changed {
			runCmd(repoRoot, gofmt, "-w", f)
		}
		if gofumpt, err := exec.LookPath("gofumpt"); err == nil {
			for _, f := range changed {
				runCmd(repoRoot, gofumpt, "-w", f)
			}
		}
		fmt.Printf("formatted %d file(s)\n", len(changed))
		rep.pass("format", "auto-fixed (gofmt -w)")
	} else {
		var badFmt []string
		for _, f := range changed {
			if formatterNeedsFix(gofmt, filepath.Join(repoRoot, filepath.FromSlash(f))) {
				badFmt = append(badFmt, f)
			}
		}
		if len(badFmt) == 0 {
			fmt.Println("gofmt OK")
			rep.pass("format", "gofmt clean")
		} else {
			fmt.Println("gofmt needs fix:")
			for _, f := range badFmt {
				fmt.Printf("  %s\n", f)
			}
			rep.fail("format", strings.Join(badFmt, ", "))
		}
		if gofumpt, err := exec.LookPath("gofumpt"); err == nil {
			var badFumpt []string
			for _, f := range changed {
				if formatterNeedsFix(gofumpt, filepath.Join(repoRoot, filepath.FromSlash(f))) {
					badFumpt = append(badFumpt, f)
				}
			}
			if len(badFumpt) == 0 {
				fmt.Println("gofumpt OK")
			} else {
				fmt.Println("gofumpt needs fix:")
				for _, f := range badFumpt {
					fmt.Printf("  %s\n", f)
				}
				rep.warn("gofumpt", strings.Join(badFumpt, ", "))
			}
		} else {
			fmt.Println("gofumpt not installed, skipped")
		}
	}

	// ---------- 4. go vet ----------
	step("go vet ./...")
	if out, errOut, c := runCmd(moduleDir, "go", "vet", "./..."); c == 0 {
		fmt.Println("vet OK")
		rep.pass("go vet", "")
	} else {
		printCmdOutput(out, errOut)
		rep.fail("go vet", "see output above")
	}

	// ---------- 5. go build ----------
	// 输出到临时目录：go build 不带 -o 会把产物写进当前目录，产物名取自模块名，
	// 若模块/目录名恰好是 go（如本仓库的 go/ 子目录），落下的 go.exe 会在
	// Windows 上遮蔽 go 工具链本身（LookPath 优先搜当前目录）。
	step("go build ./...")
	buildOut, buildErr, c := "", "", 0
	if tmpDir, err := os.MkdirTemp("", "httpbench-build-*"); err == nil {
		defer os.RemoveAll(tmpDir)
		buildOut, buildErr, c = runCmd(moduleDir, "go", "build", "-o", tmpDir, "./...")
	} else {
		buildErr, c = err.Error(), -1
	}
	if c == 0 {
		fmt.Println("build OK")
		rep.pass("go build", "")
	} else {
		printCmdOutput(buildOut, buildErr)
		rep.fail("go build", "see output above")
	}

	// ---------- 6. go test + 覆盖率 ----------
	step("go test with coverage")
	covFile := filepath.Join(moduleDir, "coverage.out")
	_ = os.Remove(covFile)
	testOut, testErr, c := runCmd(moduleDir, "go", "test", "-gcflags=all=-l", "-coverprofile=coverage.out", "./...")
	if c != 0 {
		printCmdOutput(testOut, testErr)
		rep.fail("go test", "test failed")
	} else {
		tail := splitLines(testOut)
		if len(tail) > 5 {
			tail = tail[len(tail)-5:]
		}
		for _, l := range tail {
			if strings.TrimSpace(l) != "" {
				fmt.Println(l)
			}
		}
		rep.pass("go test", "all packages passed")
	}

	// ---------- 7. 解析 coverage.out，计算全量覆盖率 ----------
	// 行格式: path/to/file.go:12.34,15.6 3 1
	covMap := map[string]map[int]bool{}
	if data, err := os.ReadFile(covFile); err == nil {
		for _, l := range splitLines(string(data)) {
			m := covLineRe.FindStringSubmatch(l)
			if m == nil {
				continue
			}
			file := m[1]
			if module != "" && strings.HasPrefix(file, module+"/") {
				file = file[len(module)+1:]
			}
			s, _ := strconv.Atoi(m[2])
			e, _ := strconv.Atoi(m[3])
			cnt, _ := strconv.Atoi(m[4])
			if covMap[file] == nil {
				covMap[file] = map[int]bool{}
			}
			for i := s; i <= e; i++ {
				if cnt > 0 {
					covMap[file][i] = true
				} else if _, ok := covMap[file][i]; !ok {
					covMap[file][i] = false
				}
			}
		}
	}

	totalStmt, coveredStmt := 0, 0
	for _, m := range covMap {
		for _, v := range m {
			totalStmt++
			if v {
				coveredStmt++
			}
		}
	}
	if totalStmt > 0 {
		fullCov := float64(coveredStmt) * 100 / float64(totalStmt)
		msg := fmt.Sprintf("total coverage: %.2f%% (%d/%d)", fullCov, coveredStmt, totalStmt)
		if *minCoverage > 0 && fullCov < *minCoverage {
			fmt.Printf("%s  < %g%%\n", msg, *minCoverage)
			rep.fail("coverage", msg)
		} else {
			fmt.Println(msg)
			rep.pass("coverage", msg)
		}
	}

	// ---------- 8. 增量覆盖率（改动行的语句覆盖） ----------
	step("incremental coverage (changed lines)")
	if *base == "HEAD" {
		fmt.Println("no base branch, skipped")
	} else {
		diffRaw, _, _ := runCmd(repoRoot, "git", "diff", "-U0", "--no-color", *base, "--", "*.go")
		addedLines := parseDiffAddedLines(diffRaw)

		dTotal, dCov, dSkipped := 0, 0, 0
		for repoRel, lines := range addedLines {
			if !*includeTests && strings.HasSuffix(repoRel, "_test.go") {
				continue
			}
			moduleRel, ok := toModuleRel(repoRel)
			if !ok {
				dSkipped += len(lines)
				continue
			}
			m := covMap[moduleRel]
			if m == nil {
				dSkipped += len(lines)
				continue
			}
			for _, ln := range lines {
				if v, ok := m[ln]; ok {
					dTotal++
					if v {
						dCov++
					}
				}
			}
		}
		if dTotal > 0 {
			pct := float64(dCov) * 100 / float64(dTotal)
			msg := fmt.Sprintf("changed lines covered: %.2f%% (%d/%d statement-lines)", pct, dCov, dTotal)
			if *minDiffCoverage > 0 && pct < *minDiffCoverage {
				fmt.Printf("%s  < %g%%\n", msg, *minDiffCoverage)
				rep.fail("diff coverage", msg)
			} else {
				fmt.Println(msg)
				rep.pass("diff coverage", msg)
			}
		} else {
			fmt.Printf("no measurable statement lines in diff (skipped %d)\n", dSkipped)
		}
	}

	// ---------- 9. 新增非测试行数 ----------
	step("added non-test lines")
	if *base != "HEAD" {
		numstat, _, _ := runCmd(repoRoot, "git", "diff", "--numstat", *base, "--", ".", ":(exclude)*_test.go")
		added := 0
		for _, l := range splitLines(numstat) {
			if m := numstatRe.FindStringSubmatch(l); m != nil {
				n, _ := strconv.Atoi(m[1])
				added += n
			}
		}
		// 未跟踪的非测试 .go 文件整文件计入
		for _, f := range untracked {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			if data, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(f))); err == nil {
				added += countLines(data)
			}
		}
		msg := fmt.Sprintf("added non-test lines: %d", added)
		if *maxAddedLines > 0 && added > *maxAddedLines {
			fmt.Printf("%s  > %d\n", msg, *maxAddedLines)
			rep.fail("line limit", msg)
		} else {
			fmt.Println(msg)
			if *maxAddedLines > 0 {
				rep.pass("line limit", msg)
			}
		}
	}

	// ---------- 10. golangci-lint ----------
	if !*skipLint {
		step("golangci-lint")
		if lintPath, err := exec.LookPath("golangci-lint"); err == nil {
			if out, errOut, c := runCmd(moduleDir, lintPath, "run", "./..."); c == 0 {
				fmt.Println("lint OK")
				rep.pass("lint", "")
			} else {
				printCmdOutput(out, errOut)
				rep.fail("lint", "see output above")
			}
		} else {
			fmt.Println("golangci-lint not installed, skipped")
		}
	}

	// ---------- 汇总 ----------
	fmt.Println("\n================ SUMMARY ================")
	for _, p := range rep.passed {
		fmt.Printf("  PASS  %s %s\n", p.name, p.detail)
	}
	for _, w := range rep.warned {
		fmt.Printf("  WARN  %s %s\n", w.name, w.detail)
	}
	for _, f := range rep.failed {
		fmt.Printf("  FAIL  %s %s\n", f.name, f.detail)
	}
	fmt.Println("========================================")
	if len(rep.failed) > 0 {
		fmt.Printf("%d check(s) failed.\n", len(rep.failed))
		return 1
	}
	fmt.Println("All checks passed.")
	return 0
}
