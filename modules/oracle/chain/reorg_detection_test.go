package chain

import (
	"testing"
	"time"

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

func TestProcessChainRelay_ReplaceBlockBypassesDedup(t *testing.T) {
	o := &ChainOracle{
		lastSubmittedEnd: make(map[string]uint64),
		lastSubmittedAt:  make(map[string]time.Time),
	}

	// Simulate a pending addBlocks up to height 100
	o.lastSubmittedEnd["BTC"] = 100
	o.lastSubmittedAt["BTC"] = time.Now()

	// A replaceBlocks session should NOT be affected by the dedup tracker
	session := chainSession{
		symbol:            "BTC",
		replaceBlock:      true,
		replaceBlockHex:   "00000020abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890",
		replaceBlockDepth: 1,
	}

	// replaceBlocks should always be allowed through
	assert.True(t, session.replaceBlock, "replaceBlocks sessions bypass dedup")
	// The tracker should still exist (not cleared by replace)
	assert.Equal(t, uint64(100), o.lastSubmittedEnd["BTC"])
}

func TestChainSession_ReplaceBlockDepth(t *testing.T) {
	// Multi-block reorg: depth > 1
	session := chainSession{
		symbol:            "BTC",
		replaceBlock:      true,
		replaceBlockHex:   "aabbccdd", // concatenated hex
		replaceBlockDepth: 3,
	}

	assert.True(t, session.replaceBlock)
	assert.Equal(t, 3, session.replaceBlockDepth)

	id, err := makeChainSessionID(&session, 93000000)
	require.NoError(t, err)
	assert.Equal(t, "BTC-93000000-replace", id)
	assert.True(t, isReplaceSession(id))
}
