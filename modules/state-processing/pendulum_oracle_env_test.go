package state_engine_test

import (
	"fmt"
	"testing"

	"vsc-node/modules/db/vsc/hive_blocks"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vsc-eco/hivego"
)

func TestPendulumOracleEnv_ExposesTickSnapshot(t *testing.T) {
	te := newTestEnv()

	for bh := uint64(1); bh <= 100; bh++ {
		block := hive_blocks.HiveBlock{
			BlockNumber:  bh,
			BlockID:      fmt.Sprintf("block-%d", bh),
			Witness:      "alice",
			Timestamp:    "2026-01-01T00:00:00",
			Transactions: nil,
		}
		if bh == 1 {
			block.Transactions = []hive_blocks.Tx{
				{
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
				},
			}
		}

		te.SE.ProcessBlock(block)
	}

	env := te.SE.PendulumOracleEnv()
	require.NotNil(t, env)

	assert.Equal(t, uint64(100), env["pendulum.tick_block_height"])
	assert.Equal(t, true, env["pendulum.hbd_interest_rate_ok"])
	assert.Equal(t, 1500, env["pendulum.hbd_interest_rate_bps"])
	assert.Equal(t, true, env["pendulum.trusted_hive_mean_ok"])
	assert.InDelta(t, 0.25, env["pendulum.trusted_hive_mean_hbd"], 1e-9)

	group, ok := env["pendulum.trusted_witness_group"].([]string)
	require.True(t, ok)
	assert.Equal(t, []string{"alice"}, group)
}

func TestPendulumOracleEnv_NoTrustedFeed(t *testing.T) {
	te := newTestEnv()

	for bh := uint64(1); bh <= 100; bh++ {
		te.SE.ProcessBlock(hive_blocks.HiveBlock{
			BlockNumber:  bh,
			BlockID:      fmt.Sprintf("block-%d", bh),
			Witness:      "",
			Timestamp:    "2026-01-01T00:00:00",
			Transactions: nil,
		})
	}

	env := te.SE.PendulumOracleEnv()
	require.NotNil(t, env)
	assert.Equal(t, uint64(100), env["pendulum.tick_block_height"])
	assert.Equal(t, false, env["pendulum.hbd_interest_rate_ok"])
	assert.Equal(t, false, env["pendulum.trusted_hive_mean_ok"])
}

