package elections_test

import (
	"testing"
	"vsc-node/lib/test_utils"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/common/consensusversion"
	"vsc-node/modules/config"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"
	"vsc-node/modules/db/vsc/elections"

	"github.com/stretchr/testify/assert"
)

func TestElectionMinimum(t *testing.T) {
	// 0.2.0+ path: fixed 2/3 quorum (no decay).
	v020 := consensusversion.V0_2_0
	val := elections.MinimalRequiredElectionVotes(0, 0, v020)
	assert.Equal(t, uint64(0), val)
	val = elections.MinimalRequiredElectionVotes(0, 8, v020)
	assert.Equal(t, uint64(6), val) // ceil(2*8/3)=6
	val = elections.MinimalRequiredElectionVotes(0, 9, v020)
	assert.Equal(t, uint64(6), val) // ceil(2*9/3)=6
	val = elections.MinimalRequiredElectionVotes(0, 100, v020)
	assert.Equal(t, uint64(67), val) // ceil(2*100/3)=67
	val = elections.MinimalRequiredElectionVotes(0, 101, v020)
	assert.Equal(t, uint64(68), val) // ceil(2*101/3)=68

	// Pre-0.2.0 path: decay to bare majority over time.
	v010 := consensusversion.Version{Major: 0, Consensus: 1, NonConsensus: 0}
	// Fresh (< 1 hour): 2/3
	val = elections.MinimalRequiredElectionVotes(0, 100, v010)
	assert.Equal(t, uint64(67), val) // ceil(2*100/3)=67
	// Fully stale (≥ 2 weeks): bare majority
	val = elections.MinimalRequiredElectionVotes(403200, 100, v010)
	assert.Equal(t, uint64(51), val) // floor(100/2+1)=51
	val = elections.MinimalRequiredElectionVotes(403200, 8, v010)
	assert.Equal(t, uint64(5), val) // floor(8/2+1)=5
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
