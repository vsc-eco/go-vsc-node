package state_engine

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	blocksProcessed = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "magi", Subsystem: "state",
		Name: "blocks_processed_total",
		Help: "Total Hive blocks processed by the state engine",
	})
	blockProcessDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: "magi", Subsystem: "state",
		Name:    "block_process_duration_seconds",
		Help:    "Histogram of state engine ProcessBlock durations",
		Buckets: prometheus.ExponentialBuckets(0.001, 2, 14),
	})
	txBatchSize = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "magi", Subsystem: "state",
		Name: "tx_batch_size",
		Help: "Size of the most recently executed transaction batch",
	})
	profileBucketDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "magi", Subsystem: "state",
		Name: "profile_bucket_duration_seconds",
		Help: "Per-call duration of state-engine profiling buckets " +
			"(ProcessBlock phases, per-tx work, sub-operations, prefetcher fetches), labeled by bucket name",
		Buckets: []float64{
			0.00001, 0.00005, 0.0001, 0.0005,
			0.001, 0.005, 0.01, 0.05,
			0.1, 0.5, 1, 5,
			10, 30, 60,
		},
	}, []string{"bucket"})
	prefetchQueued = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "magi", Subsystem: "state",
		Name: "prefetch_queued_total",
		Help: "Total CIDs enqueued to the block prefetcher",
	})
	prefetchQueueDepth = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "magi", Subsystem: "state",
		Name: "prefetch_queue_depth",
		Help: "Current depth of the block prefetcher work queue",
	})
	prefetchFetchErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "magi", Subsystem: "state",
		Name: "prefetch_fetch_errors_total",
		Help: "Block prefetcher fetch errors by reason",
	}, []string{"reason"})
)
