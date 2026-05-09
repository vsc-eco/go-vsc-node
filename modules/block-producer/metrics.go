package blockproducer

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	blocksProduced = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "magi", Subsystem: "blockproducer",
		Name: "blocks_produced_total",
		Help: "Total Magi blocks successfully produced and broadcast to Hive",
	})
	blockProduceFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "magi", Subsystem: "blockproducer",
		Name: "failures_total",
		Help: "Total Magi block production failures by stage",
	}, []string{"reason"})
)
