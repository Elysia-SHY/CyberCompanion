package llm

import (
	"expvar"
	"log"
	"sync"
	"time"
)

// ─── 指标 ──────────────────────────────────────────────────────────────────────
//
// 用标准库 expvar 暴露计数，不引第三方依赖也够用：
// /debug/vars 可以直接看，后续也能被 Prometheus expvar exporter 采集。

var (
	metricRequests = expvar.NewInt("llm_requests")
	metricFailures = expvar.NewInt("llm_failures")
	metricRetries  = expvar.NewInt("llm_retries")
)

// Requests / Failures / Retries 返回累计计数，供健康检查与排障使用。
func Requests() int64 { return metricRequests.Value() }
func Failures() int64 { return metricFailures.Value() }
func Retries() int64  { return metricRetries.Value() }

func countRequest() { metricRequests.Add(1) }
func countFailure() { metricFailures.Add(1) }

// ─── 熔断器全局实例 ────────────────────────────────────────────────────────────

var (
	breakerMu     sync.Mutex
	globalBreaker = newBreaker(5, 60*time.Second)
)

func recordSuccess() {
	breakerMu.Lock()
	defer breakerMu.Unlock()
	globalBreaker.success()
}

func recordFailure() {
	breakerMu.Lock()
	defer breakerMu.Unlock()
	globalBreaker.fail(time.Now())
}

// retryLog 记录一次重试，同时累加指标。
func retryLog(format string, v ...interface{}) {
	metricRetries.Add(1)
	log.Printf(format, v...)
}
