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
	// GV-H3: the decay floor is now ceil(2N/3) (the BFT-safe quorum), not the old
	// floor(N/2+1) bare majority. The threshold no longer drops over time, so the
	// stale/decayed cases below now equal their fresh 2/3 value.
	val := elections.MinimalRequiredElectionVotes(0)
	assert.Equal(t, uint64(0), val)
	val = elections.MinimalRequiredElectionVotes(8)
	assert.Equal(t, uint64(6), val) // GV-H3: ceil(2*8/3)=6
	val = elections.MinimalRequiredElectionVotes(9)
	assert.Equal(t, uint64(6), val) // GV-H3: ceil(2*9/3)=6
	val = elections.MinimalRequiredElectionVotes(100)
	assert.Equal(t, uint64(67), val) // GV-H3: ceil(2*100/3)=67
	val = elections.MinimalRequiredElectionVotes(101)
	assert.Equal(t, uint64(68), val) // GV-H3: ceil(2*101/3)=68
}

func TestGetElectionByHeight(t *testing.T) {
	config.UseMainConfigDuringTests = true
	dbConfig := db.NewDbConfig()
	dbConfig.Init()
	dbConfig.SetDbName("go-vsc-election-test")
	db := db.New(dbConfig)
	vsc := vsc.New(db, dbConfig)
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
			Key:     "test-key",
			Account: "test-account",
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

	// StoreElection defaults an empty Type to "initial" before persisting,
	// so the stored/read-back record carries Type == "initial".
	electionMock.Type = "initial"

	res, err := e.GetElectionByHeight(43)
	assert.NoError(t, err)
	assert.Equal(t, electionMock, res)
}
