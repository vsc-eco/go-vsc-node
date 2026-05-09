package tss

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	reshareSuccess = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "magi", Subsystem: "tss",
		Name: "reshare_success_total",
		Help: "Total successful reshare sessions",
	})
	reshareFailure = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "magi", Subsystem: "tss",
		Name: "reshare_failure_total",
		Help: "Total failed reshare sessions",
	})
	reshareTimeout = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "magi", Subsystem: "tss",
		Name: "reshare_timeout_total",
		Help: "Total reshare sessions that ended in timeout",
	})
	reshareDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: "magi", Subsystem: "tss",
		Name:    "reshare_duration_seconds",
		Help:    "Histogram of reshare session completion durations",
		Buckets: prometheus.ExponentialBuckets(0.5, 2, 10),
	})
	msgSendFailures = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "magi", Subsystem: "tss",
		Name: "message_send_failures_total",
		Help: "Total failed P2P message sends",
	})
	msgDrops = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "magi", Subsystem: "tss",
		Name: "message_drop_total",
		Help: "Total dropped messages (no matching session)",
	})
	msgRetries = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "magi", Subsystem: "tss",
		Name: "message_retry_total",
		Help: "Total P2P message retries",
	})
	msgSendLatency = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: "magi", Subsystem: "tss",
		Name:    "message_send_latency_seconds",
		Help:    "Histogram of P2P message send latencies",
		Buckets: prometheus.ExponentialBuckets(0.001, 2, 12),
	})
	blameCount = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "magi", Subsystem: "tss",
		Name: "blame_total",
		Help: "Total blame events attributed to each node",
	}, []string{"node"})
	banCount = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "magi", Subsystem: "tss",
		Name: "ban_total",
		Help: "Total node bans",
	})
	readinessFailures = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "magi", Subsystem: "tss",
		Name: "participant_readiness_failures_total",
		Help: "Total participant readiness check failures",
	})
)

// Metrics is a thin handle exposing typed helpers for TSS instrumentation.
// All counters/histograms back onto the default Prometheus registry.
type Metrics struct{}

var globalMetrics = &Metrics{}

func GetMetrics() *Metrics {
	return globalMetrics
}

func (m *Metrics) RecordReshareDuration(d time.Duration) {
	reshareDuration.Observe(d.Seconds())
}

func (m *Metrics) IncrementReshareSuccess() {
	reshareSuccess.Inc()
}

func (m *Metrics) IncrementReshareFailure() {
	reshareFailure.Inc()
}

func (m *Metrics) IncrementReshareTimeout() {
	reshareTimeout.Inc()
}

func (m *Metrics) IncrementMessageSendFailure() {
	msgSendFailures.Inc()
}

func (m *Metrics) IncrementMessageDrop() {
	msgDrops.Inc()
}

func (m *Metrics) IncrementMessageRetry() {
	msgRetries.Inc()
}

func (m *Metrics) RecordMessageSendLatency(latency time.Duration) {
	msgSendLatency.Observe(latency.Seconds())
}

func (m *Metrics) IncrementBlameCount(nodeAccount string) {
	blameCount.WithLabelValues(nodeAccount).Inc()
}

func (m *Metrics) IncrementBanCount() {
	banCount.Inc()
}

func (m *Metrics) IncrementParticipantReadinessFailure() {
	readinessFailures.Inc()
}
