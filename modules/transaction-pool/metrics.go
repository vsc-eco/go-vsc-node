package transactionpool

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	txIngestTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "magi", Subsystem: "txpool",
		Name: "ingest_total",
		Help: "Total transactions accepted into the local mempool",
	})
	txIngestFailures = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "magi", Subsystem: "txpool",
		Name: "ingest_failures_total",
		Help: "Total transaction ingest failures (validation, signature, etc.)",
	})
	txBroadcastFailures = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "magi", Subsystem: "txpool",
		Name: "broadcast_failures_total",
		Help: "Total mempool announce broadcast failures",
	})
)
