package p2p_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"vsc-node/experiments/p2p"
	"vsc-node/experiments/p2p/config"
	"vsc-node/experiments/p2p/discovery"
	"vsc-node/experiments/p2p/http"
	"vsc-node/experiments/p2p/peer"
	"vsc-node/experiments/p2p/peers"
	"vsc-node/experiments/p2p/status"
	"vsc-node/modules/aggregate"

	"github.com/chebyrash/promise"
)

const ManyNodeCount = 350

func TestManyNodes(t *testing.T) {
	prs := make([]*peers.Peers, ManyNodeCount)
	p2ps := make([]aggregate.Plugin, ManyNodeCount)
	for i := uint16(0); i < ManyNodeCount; i++ {
		cfg := config.NewWithConfig(config.Config{
			// bootstrap nodes
			Peers: []peer.Peer{"127.0.0.1:1447", "127.0.0.1:1448", "127.0.0.1:1449"},

			Addr: "127.0.0.1:",
			Port: 1447 + i,

			// defaults to 20 then will scan for new nodes every minute
			MinPeers: ManyNodeCount * 7 / 8,
		})
		p2ps[i] = p2p.NewWithConfig(cfg, func(config *config.Config, peers *peers.Peers, discovery *discovery.PeerDiscovery, http *http.HttpServer) aggregate.Plugin {
			prs[i] = peers
			return status.NewWithIntervalAndPrefix(fmt.Sprintf("node [%d]", i), peers, status.DefaultInterval)
		})
	}
	main := aggregate.New(p2ps)
	fmt.Println("starting nodes")
	if err := main.Run(); err != nil {
		t.Fatal(err)
	}
	promise.New(func(resolve func(struct{}), reject func(error)) {
		time.AfterFunc(7*time.Second, func() {
			if err := main.Stop(); err != nil {
				t.Fatal(err)
			}
			resolve(struct{}{})
		})
	}).Await(context.Background())
	for i, peers := range prs {
		if peers.Size() < (ManyNodeCount * 7 / 8) {
			t.Fatalf("peer [%d] did not connect to most other nodes with %d connections", i, peers.Size())
		}
	}
}
