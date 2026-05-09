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
)
