package data_availability_test

import (
	"fmt"
	"testing"
	"vsc-node/lib/test_utils"
	"vsc-node/lib/utils"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/config"
	"vsc-node/modules/db/vsc/elections"
	stateEngine "vsc-node/modules/state-processing"

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

	test_utils.RunPlugin(t, runner)

	data := []byte("some random data")

	election := elections.ElectionResult{}
	election.Members = utils.Map(servers, func(s *Node) elections.ElectionMember {
		did := s.ConsensusKey()
		return elections.ElectionMember{
			Key:     did,
			Account: "", //FIXME technically username doesn't matter here, but it might in the future
		}
	})

	assert.NoError(t, client.NukeDb())
	for _, server := range servers {
		assert.NoError(t, server.NukeDb())
	}

	res, err := client.client.RequestProofWithElection(data, election)
	assert.NoError(t, err)
	assert.Truef(t, res.Verify(election), "failed to verify data availability proof")
}
