package oraclee2e

import (
	"context"
	"fmt"
	"testing"
	"vsc-node/modules/aggregate"

	"github.com/stretchr/testify/assert"
)

func TestE2E(t *testing.T) {
	plugins := []aggregate.Plugin{}
	for i := range 10 {
		nodeName := fmt.Sprintf("testnode-%d", i)
		node := MakeNode(nodeName)
		plugins = append(plugins, append(node.plugins, node.oracle)...)
	}

	p := aggregate.New(plugins)
	assert.NoError(t, p.Init())
	/* line 565 e2e_test.go

	func() {

			peerAddrs := make([]string, 0)

			for _, node := range runningNodes {
				for _, addr := range node.P2P.Addrs() {
					peerAddrs = append(peerAddrs, addr.String()+"/p2p/"+node.P2P.ID().String())
				}
			}

			for _, node := range runningNodes {
				for _, peerStr := range peerAddrs {
					peerId, _ := peer.AddrInfoFromString(peerStr)
					ctx := context.Background()
					ctx, _ = context.WithTimeout(ctx, 5*time.Second)
					fmt.Println("Trying to connect", peerId)
					node.P2P.Connect(ctx, *peerId)
				}
			}
		}()


	*/

	_, err := p.Start().Await(context.Background())
	assert.NoError(t, err)
}
