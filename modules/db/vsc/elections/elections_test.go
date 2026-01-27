package elections_test

import (
	"testing"
	"vsc-node/lib/test_utils"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/config"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"
	"vsc-node/modules/db/vsc/elections"

	"github.com/stretchr/testify/assert"
)

func TestElectionMinimum(t *testing.T) {
	//Yes it won't fail
	val := elections.MinimalRequiredElectionVotes(elections.MIN_BLOCKS_SINCE_LAST_ELECTION-1, 0)
	assert.Equal(t, 0, val)
	val = elections.MinimalRequiredElectionVotes(elections.MAX_BLOCKS_SINCE_LAST_ELECTION, 8)
	assert.Equal(t, 5, val)
	val = elections.MinimalRequiredElectionVotes(elections.MIN_BLOCKS_SINCE_LAST_ELECTION, 8)
	assert.Equal(t, 6, val)
	val = elections.MinimalRequiredElectionVotes(elections.MIN_BLOCKS_SINCE_LAST_ELECTION, 9)
	assert.Equal(t, 6, val)
	val = elections.MinimalRequiredElectionVotes(elections.MAX_BLOCKS_SINCE_LAST_ELECTION+1, 9)
	assert.Equal(t, 5, val)

	val = elections.MinimalRequiredElectionVotes((elections.MAX_BLOCKS_SINCE_LAST_ELECTION-elections.MIN_BLOCKS_SINCE_LAST_ELECTION)/2, 100)
	assert.Equal(t, 59, val)
	val = elections.MinimalRequiredElectionVotes((elections.MAX_BLOCKS_SINCE_LAST_ELECTION-elections.MIN_BLOCKS_SINCE_LAST_ELECTION)/2, 101)

	assert.Equal(t, 60, val)
}

func TestGetElectionByHeight(t *testing.T) {
	config.UseMainConfigDuringTests = true
	dbConfig := db.NewDbConfig()
	db := db.New(dbConfig)
	vsc := vsc.New(db, "go-vsc-election-test")
	e := elections.New(vsc)

	test_utils.RunPlugin(t, aggregate.New([]aggregate.Plugin{
		dbConfig,
		db,
		vsc,
		e,
	}))

	err := vsc.Clear()
	assert.NoError(t, err)

	electionMock := elections.ElectionResult{}
	electionMock.BlockHeight = 42
	electionMock.Data = "bafy-test-data"
	electionMock.Epoch = 69
	electionMock.Members = []elections.ElectionMember{
		{
			"test-key",
			"test-account",
		},
	}
	electionMock.NetId = "test-net-id"
	electionMock.Proposer = "test-proposer-account"
	electionMock.ProtocolVersion = 12
	electionMock.TotalWeight = 42069
	electionMock.Weights = []uint64{
		42069,
	}
	electionMock.TxId = "0123456789abcdef0123456789abcdef01234567"

	err = e.StoreElection(electionMock)
	assert.NoError(t, err)

	res, err := e.GetElectionByHeight(43)
	assert.NoError(t, err)
	assert.Equal(t, electionMock, res)
}
