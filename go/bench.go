package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Config 压测配置。
type Config struct {
	URL         string        // 目标 URL
	Concurrency int           // 并发 worker 数
	Duration    time.Duration // 持续时间，<=0 表示不限时
	Total       int64         // 总请求数上限，<=0 表示不限量
	Timeout     time.Duration // 单请求超时
}

// Result 压测结果。
type Result struct {
	TotalRequests   int64         `json:"total_requests"`
	SuccessRequests int64         `json:"success_requests"`
	FailedRequests  int64         `json:"failed_requests"`
	TotalDuration   time.Duration `json:"-"`
	TotalSeconds    float64       `json:"total_seconds"`
	AvgResponseTime time.Duration `json:"-"`
	MinResponseTime time.Duration `json:"-"`
	MaxResponseTime time.Duration `json:"-"`
	P50ResponseTime time.Duration `json:"-"`
	P90ResponseTime time.Duration `json:"-"`
	P95ResponseTime time.Duration `json:"-"`
	P99ResponseTime time.Duration `json:"-"`
	QPS             float64       `json:"qps"`
	Throughput      float64       `json:"throughput_bytes_per_sec"`
	TotalBytes      int64         `json:"total_bytes"`

	// 供 JSON 输出的毫秒形式
	AvgMs float64 `json:"avg_ms"`
	MinMs float64 `json:"min_ms"`
	MaxMs float64 `json:"max_ms"`
	P50Ms float64 `json:"p50_ms"`
	P90Ms float64 `json:"p90_ms"`
	P95Ms float64 `json:"p95_ms"`
	P99Ms float64 `json:"p99_ms"`
}

// Run 执行压测并返回统计结果。
//
// 核心模型：固定 Concurrency 个 worker goroutine，每个 worker 循环发起请求，
// 直到超出 Duration 或达到 Total 上限。每个 worker 在本地收集成功请求的
// 延迟，结束后统一合并排序，避免压测过程中的全局锁竞争干扰被测服务。
func Run(cfg Config) Result {
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          cfg.Concurrency * 2,
		MaxIdleConnsPerHost:   cfg.Concurrency * 2,
		MaxConnsPerHost:       cfg.Concurrency * 2,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	client := &http.Client{
		Timeout:   cfg.Timeout,
		Transport: transport,
	}

	var (
		totalRequests   int64
		failedRequests  int64
		totalBytes      int64
		issued          int64 // 已发起的请求数（用于 -n 限量）
		successRequests int64
	)

	var endTime time.Time
	if cfg.Duration > 0 {
		endTime = time.Now().Add(cfg.Duration)
	}
	startTime := time.Now()

	// 每个 worker 本地收集延迟，结束后合并
	localTimes := make([][]time.Duration, cfg.Concurrency)

	var wg sync.WaitGroup
	for i := 0; i < cfg.Concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			times := make([]time.Duration, 0, 1024)

			for {
				// -n 限量：原子抢占一个名额，抢不到则停止
				if cfg.Total > 0 && atomic.AddInt64(&issued, 1) > cfg.Total {
					break
				}
				// -d 限时：超过截止时间则停止
				if !endTime.IsZero() && !time.Now().Before(endTime) {
					break
				}

				atomic.AddInt64(&totalRequests, 1)
				start := time.Now()
				resp, err := client.Get(cfg.URL)
				responseTime := time.Since(start)

				if err != nil {
					atomic.AddInt64(&failedRequests, 1)
					continue
				}

				body, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()

				if err != nil {
					atomic.AddInt64(&failedRequests, 1)
					continue
				}

				atomic.AddInt64(&totalBytes, int64(len(body)))
				times = append(times, responseTime)
			}
			localTimes[idx] = times
		}(i)
	}

	wg.Wait()
	totalDuration := time.Since(startTime)

	// 合并所有 worker 的延迟样本
	responseTimes := make([]time.Duration, 0, totalRequests)
	for _, times := range localTimes {
		responseTimes = append(responseTimes, times...)
	}
	sort.Slice(responseTimes, func(i, j int) bool {
		return responseTimes[i] < responseTimes[j]
	})

	successRequests = totalRequests - failedRequests
	seconds := totalDuration.Seconds()
	if seconds <= 0 {
		seconds = 1e-9
	}

	result := Result{
		TotalRequests:   totalRequests,
		SuccessRequests: successRequests,
		FailedRequests:  failedRequests,
		TotalDuration:   totalDuration,
		TotalSeconds:    seconds,
		AvgResponseTime: calculateAvg(responseTimes),
		MinResponseTime: calculateMin(responseTimes),
		MaxResponseTime: calculateMax(responseTimes),
		P50ResponseTime: calculatePercentile(responseTimes, 50),
		P90ResponseTime: calculatePercentile(responseTimes, 90),
		P95ResponseTime: calculatePercentile(responseTimes, 95),
		P99ResponseTime: calculatePercentile(responseTimes, 99),
		QPS:             float64(totalRequests) / seconds,
		Throughput:      float64(totalBytes) / seconds,
		TotalBytes:      totalBytes,
	}
	result.AvgMs = durationToMs(result.AvgResponseTime)
	result.MinMs = durationToMs(result.MinResponseTime)
	result.MaxMs = durationToMs(result.MaxResponseTime)
	result.P50Ms = durationToMs(result.P50ResponseTime)
	result.P90Ms = durationToMs(result.P90ResponseTime)
	result.P95Ms = durationToMs(result.P95ResponseTime)
	result.P99Ms = durationToMs(result.P99ResponseTime)
	return result
}

func calculateAvg(times []time.Duration) time.Duration {
	if len(times) == 0 {
		return 0
	}
	var total time.Duration
	for _, t := range times {
		total += t
	}
	return total / time.Duration(len(times))
}

func calculateMin(times []time.Duration) time.Duration {
	if len(times) == 0 {
		return 0
	}
	return times[0]
}

func calculateMax(times []time.Duration) time.Duration {
	if len(times) == 0 {
		return 0
	}
	return times[len(times)-1]
}

func calculatePercentile(times []time.Duration, percentile int) time.Duration {
	if len(times) == 0 {
		return 0
	}
	index := int(float64(percentile) / 100.0 * float64(len(times)))
	if index >= len(times) {
		index = len(times) - 1
	}
	return times[index]
}

func durationToMs(d time.Duration) float64 {
	return float64(d.Nanoseconds()) / 1e6
}

// TextReport 生成人类可读的文本报告。
func (r Result) TextReport() string {
	s := "========================================\n"
	s += "压测结果:\n"
	s += "========================================\n"
	s += fmt.Sprintf("总请求数:     %d\n", r.TotalRequests)
	s += fmt.Sprintf("成功请求数:   %d\n", r.SuccessRequests)
	s += fmt.Sprintf("失败请求数:   %d\n", r.FailedRequests)
	s += fmt.Sprintf("总耗时:       %v\n", r.TotalDuration)
	s += "\n"
	s += fmt.Sprintf("QPS:          %.2f req/s\n", r.QPS)
	s += fmt.Sprintf("吞吐量:       %.2f bytes/s\n", r.Throughput)
	s += fmt.Sprintf("总数据量:     %d bytes\n", r.TotalBytes)
	s += "\n"
	s += "响应时间:\n"
	s += fmt.Sprintf("  平均:       %v\n", r.AvgResponseTime)
	s += fmt.Sprintf("  最小:       %v\n", r.MinResponseTime)
	s += fmt.Sprintf("  最大:       %v\n", r.MaxResponseTime)
	s += fmt.Sprintf("  P50:        %v\n", r.P50ResponseTime)
	s += fmt.Sprintf("  P90:        %v\n", r.P90ResponseTime)
	s += fmt.Sprintf("  P95:        %v\n", r.P95ResponseTime)
	s += fmt.Sprintf("  P99:        %v\n", r.P99ResponseTime)
	s += "========================================\n"
	return s
}
