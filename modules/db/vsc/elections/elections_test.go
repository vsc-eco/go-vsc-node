package elections_test

import (
	"testing"
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
