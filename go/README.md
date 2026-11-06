# Go 测试工具集（httpbench）

本目录（`go/`）是 [testing_tools](https://github.com/2253690086yu-byte/testing_tools) 仓库的 Go 工具集，仓库后续会拓展其他语言的测试工具。

包含：

- **HTTP 压测**：极简的 HTTP 压测 CLI。固定并发 worker 在限定时间（或限定请求数）内循环请求目标 URL，
  输出 QPS、吞吐量与延迟分位数（P50/P90/P95/P99）。
- **提交前自检**：`check` 子命令（移植自 go-check.ps1），对 Go 项目做格式 / vet / build / test / 覆盖率 / lint 自检。

零第三方依赖，单个二进制，Windows/Linux/macOS 均可运行。

从 广告投放服务的接口压测实践中提炼而来。

## 功能特性

- 固定并发模型：`-c` 个 worker 循环发请求，直到达到时长 `-d` 或总量 `-n`
- 完整延迟统计：平均 / 最小 / 最大 / P50 / P90 / P95 / P99
- 吞吐量统计：QPS、bytes/s、总数据量
- 双输出格式：人类可读文本报告、`-json` 结构化输出（便于脚本/CI 消费）
- 压测干扰最小化：每个 worker 本地收集延迟样本，结束后统一合并排序，无全局锁竞争
- 失败请求非零退出码，可直接接入 CI

## 安装

```bash
# 方式一：go install（需 Go 1.24+；模块位于仓库 go/ 子目录，路径带 /go 后缀）
go install github.com/2253690086yu-byte/testing_tools/go@latest

# 方式二：源码构建
git clone https://github.com/2253690086yu-byte/testing_tools.git
cd testing_tools/go
go build -o httpbench .
```

## 快速开始

```bash
# 50 并发压测 30 秒（默认）
httpbench -url=http://localhost:8005/api/v1/items?user_id=1&count=20

# 100 并发压测 60 秒，结果保存为 JSON
httpbench -url=http://192.0.2.10:8005/ -c=100 -d=60s -json -o=result.json

# 限量 10000 个请求（不限时）
httpbench -url=http://localhost:8005/ -c=20 -n=10000 -d=0
```

## 参数说明

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-url` | （必填） | 请求 URL |
| `-c` | 50 | 并发 worker 数 |
| `-d` | 30s | 压测持续时间；`0` 表示不限时，需配合 `-n` |
| `-n` | 0 | 总请求数上限；`0` 表示不限量；与 `-d` 同时生效，先到者停止 |
| `-timeout` | 10s | 单请求超时 |
| `-json` | false | 以 JSON 格式输出结果 |
| `-o` | （stdout） | 结果写入指定文件 |

## 输出示例

文本报告：

```
========================================
压测结果:
========================================
总请求数:     127341
成功请求数:   127341
失败请求数:   0
总耗时:       30.041s

QPS:          4241.73 req/s
吞吐量:       2756543.20 bytes/s
总数据量:     82800512 bytes

响应时间:
  平均:       47.13ms
  最小:       1.2ms
  最大:       308ms
  P50:        21.33ms
  P90:        94.32ms
  P95:        100.12ms
  P99:        154.57ms
========================================
```

JSON 报告（`-json`）：

```json
{
  "total_requests": 127341,
  "success_requests": 127341,
  "failed_requests": 0,
  "total_seconds": 30.041,
  "qps": 4241.73,
  "throughput_bytes_per_sec": 2756543.2,
  "total_bytes": 82800512,
  "avg_ms": 47.13,
  "min_ms": 1.2,
  "max_ms": 308.0,
  "p50_ms": 21.33,
  "p90_ms": 94.32,
  "p95_ms": 100.12,
  "p99_ms": 154.57
}
```

**退出码**：`0` 全部成功；`1` 存在失败请求或写文件失败；`2` 参数错误。

## 压测方法论

以下经验来自 广告投放服务的实际压测：

1. **梯度压测找拐点**：以 10/20/50/100/200 的并发梯度递增压测，观察 QPS 与 P99 的变化曲线。
   QPS 不再增长而延迟线性上升的临界并发，即为服务的吞吐瓶颈。
2. **用 P99 而非平均值定容量**：以「P99 < SLA（如 300ms）」为约束反推最大 QPS，
   平均值会掩盖长尾。拐点之后 P99 往往率先劣化（如从 200ms 跳到 584ms）。
3. **拐点并发做长稳验证**：在候选最优并发下持续 60s 以上，确认 QPS 波动 < 5%、
   无失败请求、P99 稳定，再给出容量结论。
4. **容量规划公式**：所需实例数 = 目标 QPS / 单机最大 QPS，再乘 1.2 冗余。

## 提交前自检：check 子命令

`check`（别名 `lint`）是 Go 项目提交前自检，只依赖 `go` + `git`：

```bash
httpbench check                                                   # 默认检查
httpbench check -base origin/main -min-coverage 68 -min-diff-coverage 50
httpbench check -fix                                              # 自动格式化本次改动的文件
```

在包含 `go.mod` 的目录（或其子目录）下运行，自动定位 git 仓库根与模块根。检查项：

1. **gofmt 格式**（CRLF 安全，不会误报；装了 gofumpt 则附加检查，仅告警）
2. **go vet**
3. **go build**
4. **go test** + 全量语句覆盖率
5. **增量覆盖率**（相对基线分支的改动行，自行解析 coverage.out，不需要 diff-cover）
6. **非测试代码新增行数上限**
7. **golangci-lint**（若已安装）

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-base` | 自动探测 | 基线分支（依次探测 origin/main、origin/master、main、master、origin/develop，失败则用 HEAD） |
| `-min-coverage` | 0 | 全量语句覆盖率下限，0 表示不检查 |
| `-min-diff-coverage` | 0 | 增量语句覆盖率下限，0 表示不检查 |
| `-max-added-lines` | 0 | 非测试代码新增行数上限，0 表示不检查 |
| `-fix` | false | 自动格式化本次改动的文件（gofmt -w） |
| `-skip-lint` | false | 跳过 golangci-lint |
| `-include-tests` | false | 增量覆盖率统计包含 `_test.go` |

退出码：`0` 全部通过；`1` 存在失败项。

## 与 hey / wrk 的区别

| 维度 | httpbench | hey / wrk |
|------|-----------|-----------|
| 依赖 | 零依赖单二进制 | wrk 需 Lua 环境 |
| 输出 | 中文文本报告 + JSON | 英文 |
| 停止条件 | 时长 / 请求总数 任一 | 时长或总数 |
| 适用场景 | 快速接口容量评估、报告留档 | 更复杂的场景（自定义脚本、多 URL） |

## License

Internal use.
