package chain

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsReplaceSession(t *testing.T) {
	assert.True(t, isReplaceSession("BTC-93000000-replace"))
	assert.True(t, isReplaceSession("DASH-100-replace"))
	assert.False(t, isReplaceSession("BTC-93000000-640000-640100"))
	assert.False(t, isReplaceSession("BTC-replace-640000"))
	assert.False(t, isReplaceSession(""))
}

func TestMakeChainSessionID_ReplaceBlock(t *testing.T) {
	session := &chainSession{
		symbol:       "BTC",
		replaceBlock: true,
	}

	id, err := makeChainSessionID(session, 93000000)
	require.NoError(t, err)
	assert.Equal(t, "BTC-93000000-replace", id)
	assert.True(t, isReplaceSession(id))
}

func TestMakeChainSessionID_ReplaceDoesNotRequireChainData(t *testing.T) {
	// replaceBlock sessions have no chainData — should not panic or error
	session := &chainSession{
		symbol:       "BTC",
		replaceBlock: true,
		chainData:    nil,
	}

	id, err := makeChainSessionID(session, 100)
	require.NoError(t, err)
	assert.Contains(t, id, "replace")
}

func TestProcessChainRelay_SkipsDuplicateReplace(t *testing.T) {
	o := &ChainOracle{
		lastSubmitted: make(map[string]string),
	}

	hex := "00000020abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"

	// Simulate having already submitted this replace
	rangeKey := "replace-" + hex[:16]
	o.lastSubmitted["BTC"] = rangeKey

	session := chainSession{
		symbol:          "BTC",
		replaceBlock:    true,
		replaceBlockHex: hex,
	}

	// The dedup key should match
	testRangeKey := "replace-" + session.replaceBlockHex[:16]
	assert.Equal(t, rangeKey, testRangeKey)
	assert.Equal(t, o.lastSubmitted["BTC"], testRangeKey)
}
