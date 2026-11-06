// httpbench 是一个极简的 HTTP 压测 CLI。
//
// 固定数量的并发 worker 在限定时间（或限定请求数）内循环请求目标 URL，
// 统计 QPS、吞吐量与延迟分位数（P50/P90/P95/P99），支持文本与 JSON 两种输出。
//
// 另含 check 子命令：Go 项目提交前自检（gofmt / vet / build / test / 覆盖率 / lint）。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"
)

func main() {
	// check（别名 lint）子命令：Go 项目提交前自检
	if len(os.Args) > 1 && (os.Args[1] == "check" || os.Args[1] == "lint") {
		os.Exit(runCheck(os.Args[2:]))
	}

	var (
		url         = flag.String("url", "", "请求 URL（必填）")
		concurrency = flag.Int("c", 50, "并发 worker 数")
		duration    = flag.Duration("d", 30*time.Second, "压测持续时间（0 表示不限时，需配合 -n）")
		total       = flag.Int64("n", 0, "总请求数上限（0 表示不限量），与 -d 同时生效，先到者停止")
		timeout     = flag.Duration("timeout", 10*time.Second, "单请求超时")
		jsonOut     = flag.Bool("json", false, "以 JSON 格式输出结果")
		outFile     = flag.String("o", "", "结果写入指定文件（默认输出到 stdout）")
	)
	flag.Parse()

	if *url == "" {
		fmt.Fprintln(os.Stderr, "请指定请求 URL: httpbench -url=http://...")
		fmt.Fprintln(os.Stderr, "或使用提交前自检子命令: httpbench check")
		flag.Usage()
		os.Exit(2)
	}
	if *concurrency <= 0 {
		fmt.Fprintln(os.Stderr, "并发数 -c 必须大于 0")
		os.Exit(2)
	}
	if *duration <= 0 && *total <= 0 {
		fmt.Fprintln(os.Stderr, "-d 与 -n 至少设置一个")
		os.Exit(2)
	}

	fmt.Fprintf(os.Stderr, "开始压测: url=%s 并发=%d 时长=%v 总量=%d\n",
		*url, *concurrency, *duration, *total)

	result := Run(Config{
		URL:         *url,
		Concurrency: *concurrency,
		Duration:    *duration,
		Total:       *total,
		Timeout:     *timeout,
	})

	var report []byte
	if *jsonOut {
		b, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "JSON 序列化失败: %v\n", err)
			os.Exit(1)
		}
		report = append(b, '\n')
	} else {
		report = []byte(result.TextReport())
	}

	if *outFile != "" {
		if err := os.WriteFile(*outFile, report, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "写入文件失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "结果已写入 %s\n", *outFile)
	} else {
		fmt.Print(string(report))
	}

	// 存在失败请求时以非零码退出，便于 CI 集成
	if result.FailedRequests > 0 {
		os.Exit(1)
	}
}
