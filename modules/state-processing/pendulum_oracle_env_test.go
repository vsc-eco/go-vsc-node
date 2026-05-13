package state_engine_test

import (
	"fmt"
	"testing"

	"vsc-node/modules/db/vsc/hive_blocks"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vsc-eco/hivego"
)

// pumpHiveBlocks feeds [from, to] sequential blocks through ProcessBlock.
// publishHeights names the heights at which "alice" publishes a 0.25 HBD per
// 1.000 HIVE feed_publish; ProcessBlock injects the op exactly once per
// listed height. All other blocks just record the witness.
func pumpHiveBlocks(t *testing.T, te *testEnv, from, to uint64, publishHeights ...uint64) {
	t.Helper()
	publishSet := make(map[uint64]struct{}, len(publishHeights))
	for _, h := range publishHeights {
		publishSet[h] = struct{}{}
	}
	for bh := from; bh <= to; bh++ {
		block := hive_blocks.HiveBlock{
			BlockNumber: bh,
			BlockID:     fmt.Sprintf("block-%d", bh),
			Witness:     "alice",
			Timestamp:   "2026-01-01T00:00:00",
		}
		if _, ok := publishSet[bh]; ok {
			block.Transactions = []hive_blocks.Tx{{
				Operations: []hivego.Operation{
					{
						Type: "feed_publish",
						Value: map[string]interface{}{
							"publisher": "alice",
							"exchange_rate": map[string]interface{}{
								"base":  "0.25 HBD",
								"quote": "1.000 HIVE",
							},
						},
					},
					{
						Type: "witness_set_properties",
						Value: map[string]interface{}{
							"owner": "alice",
							"props": []interface{}{
								[]interface{}{"hbd_interest_rate", "1500"},
							},
						},
					},
				},
			}}
		}
		te.SE.ProcessBlock(block)
	}
}

// TestPendulumOracleEnv_NotWarmed_ReturnsNil verifies the warmup gate: until
// the FeedTracker's signature window and MA ring are full (the natural-fill
// path of Warmed() — Warmup() is not invoked here because the test env wires
// hiveBlocks=nil), the env stays nil to keep contracts from observing
// per-node-divergent partial state.
func TestPendulumOracleEnv_NotWarmed_ReturnsNil(t *testing.T) {
	te := newTestEnv()

	// One tick fires at bh=100 with alice published — but the MA ring needs
	// three trusted-price ticks to fill, so Warmed() stays false.
	pumpHiveBlocks(t, te, 1, 100, 1)

	require.False(t, te.SE.PendulumFeedTracker().Warmed())
	assert.Nil(t, te.SE.PendulumOracleEnv())
}

// TestPendulumOracleEnv_ExposesTickSnapshot drives the tracker through three
// trusted-price ticks (100, 200, 300) so both the signature window (100
// blocks of "alice" producing) and the MA ring (3 entries) fill, then asserts
// the env exposes the latest tick's snapshot.
func TestPendulumOracleEnv_ExposesTickSnapshot(t *testing.T) {
	te := newTestEnv()

	// Publish at every tick boundary so each tick's trust check passes
	// (lastFeedBlk[alice] sits inside the rolling 100-block window).
	pumpHiveBlocks(t, te, 1, 300, 1, 100, 200, 300)

	require.True(t, te.SE.PendulumFeedTracker().Warmed())

	env := te.SE.PendulumOracleEnv()
	require.NotNil(t, env)

	assert.Equal(t, uint64(300), env["pendulum.tick_block_height"])
	assert.Equal(t, true, env["pendulum.hbd_interest_rate_ok"])
	assert.Equal(t, 1500, env["pendulum.hbd_interest_rate_bps"])
	assert.Equal(t, true, env["pendulum.trusted_hive_mean_ok"])
	// 0.25 HBD per 1 HIVE = 2500 bps (allow ±1 base unit for integer-floor noise).
	priceBps, ok := env["pendulum.trusted_hive_price_bps"].(int64)
	require.True(t, ok)
	assert.GreaterOrEqual(t, priceBps, int64(2_499))
	assert.LessOrEqual(t, priceBps, int64(2_501))

	group, ok := env["pendulum.trusted_witness_group"].([]string)
	require.True(t, ok)
	assert.Equal(t, []string{"alice"}, group)
}

// TestPendulumOracleEnv_WarmedButFeedAgedOut covers the post-warmup state
// where the tracker has filled both rings but no trusted feed has published
// inside the most recent rolling-100 window. Warmed() stays true (the rings
// don't drain), but the tick snapshot reports trusted_hive_mean_ok=false so
// contracts know the price isn't current.
func TestPendulumOracleEnv_WarmedButFeedAgedOut(t *testing.T) {
	te := newTestEnv()

	// Warm the tracker through tick 300, then run two more ticks (400, 500)
	// without further publishes — alice's last publish at bh=300 ages out of
	// the trust window (300+100=400 not > 500) by the time tick 500 fires.
	pumpHiveBlocks(t, te, 1, 500, 1, 100, 200, 300)

	require.True(t, te.SE.PendulumFeedTracker().Warmed())

	env := te.SE.PendulumOracleEnv()
	require.NotNil(t, env)
	assert.Equal(t, uint64(500), env["pendulum.tick_block_height"])
	assert.Equal(t, false, env["pendulum.trusted_hive_mean_ok"])
	// APR is sourced from the running trusted witness group at this tick;
	// with no trusted witnesses, the APR aggregator returns ok=false even
	// though witnessProps still holds alice's bh=300 advertisement.
	assert.Equal(t, false, env["pendulum.hbd_interest_rate_ok"])
}
