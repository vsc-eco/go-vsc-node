package libp2p

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

var registerPeerGaugeOnce sync.Once

// registerPeerGauge wires a Prometheus GaugeFunc that reports the number of
// libp2p peers currently connected to this node. Safe to call multiple times;
// only the first invocation registers a collector.
func (p2pServer *P2PServer) registerPeerGauge() {
	registerPeerGaugeOnce.Do(func() {
		_ = prometheus.DefaultRegisterer.Register(prometheus.NewGaugeFunc(
			prometheus.GaugeOpts{
				Namespace: "magi", Subsystem: "p2p",
				Name: "connected_peers",
				Help: "Current count of libp2p peers connected to this node",
			},
			func() float64 {
				if p2pServer.host == nil {
					return 0
				}
				return float64(len(p2pServer.host.Network().Peers()))
			},
		))
	})
}
