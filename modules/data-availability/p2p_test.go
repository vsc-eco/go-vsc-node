package data_availability_test

import (
	"context"
	"fmt"
	"testing"
	"time"
	"vsc-node/lib/test_utils"
	"vsc-node/lib/utils"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/config"
	"vsc-node/modules/db/vsc/elections"
	stateEngine "vsc-node/modules/state-processing"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/stretchr/testify/assert"
)

func TestBasicP2P(t *testing.T) {
	config.UseMainConfigDuringTests = true
	// libp2p.BOOTSTRAP = []string{} // this is popluated by `MakeNode`

	client := MakeNode(MakeNodeInput{
		Username: "client",
		Client:   true,
	})

	servers := make([]*Node, stateEngine.STORAGE_PROOF_MINIMUM_SIGNERS)
	for i := range servers {
		servers[i] = MakeNode(MakeNodeInput{
			Username: fmt.Sprint("server-", i),
		})
	}

	runner := aggregate.New(append(
		utils.Map(servers, func(s *Node) aggregate.Plugin {
			return s
		}),
		client,
	))

	test_utils.RunPlugin(t, runner, false)

	peerAddrs := make([]string, 0)

	for _, node := range servers {
		for _, addr := range node.p2p.Addrs() {
			peerAddrs = append(peerAddrs, addr.String()+"/p2p/"+node.p2p.ID().String())
		}
	}
	for _, peerStr := range peerAddrs {
		peerId, _ := peer.AddrInfoFromString(peerStr)
		ctx := context.Background()
		ctx, _ = context.WithTimeout(ctx, 5*time.Second)
		client.p2p.Connect(ctx, *peerId)
	}

	time.Sleep(time.Second)

	data := []byte("some random data")

	election := elections.ElectionResult{}
	election.Members = utils.Map(servers, func(s *Node) elections.ElectionMember {
		did := s.ConsensusKey()
		return elections.ElectionMember{
			Key:     did,
			Account: "", //FIXME technically username doesn't matter here, but it might in the future
		}
	})
	election.Weights = utils.Map(servers, func(s *Node) uint64 {
		return 1
	})
	election.TotalWeight = uint64(len(servers))
	election.BlockHeight = 0

	assert.NoError(t, client.NukeDb())
	for _, server := range servers {
		assert.NoError(t, server.NukeDb())
	}

	res, err := client.client.RequestProofWithElection(data, election)
	assert.NoError(t, err)
	assert.Truef(t, res.Verify(election), "failed to verify data availability proof")
}
