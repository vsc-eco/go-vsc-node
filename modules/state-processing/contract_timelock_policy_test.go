package state_engine

import (
	"testing"

	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"

	"github.com/stretchr/testify/assert"
)

// timelockHeightSconf wraps a base SystemConfig so a test can override the
// mainnet rollout gate (ConsensusParams.Version0_2_0Height) without a
// JSON override loader — mirroring the testChecksumSconf pattern used for the
// EVM-checksum gate. OnMainnet()/ContractUpdateTimelockBlocks() fall through to
// the embedded base.
type timelockHeightSconf struct {
	systemconfig.SystemConfig
	cp params.ConsensusParams
}

func (t *timelockHeightSconf) ConsensusParams() params.ConsensusParams { return t.cp }

func mainnetWithTimelockHeight(h uint64) systemconfig.SystemConfig {
	base := systemconfig.MainnetConfig()
	cp := base.ConsensusParams()
	cp.Version0_2_0Height = h
	return &timelockHeightSconf{SystemConfig: base, cp: cp}
}

// TestContractUpdateActivationHeight covers the network-aware, consensus-gated
// timelock policy that decides when a queued update activates. The duration is
// network-baked (non-overridable) and, on mainnet, only applies at/after the
// rollout gate height so historical reindex stays byte-identical.
func TestContractUpdateActivationHeight(t *testing.T) {
	const submit = uint64(200_000_000)

	// Mocknet: timelock disabled (0 blocks) -> immediate.
	mock := &StateEngine{sconf: systemconfig.MocknetConfig()}
	assert.Equal(t, submit, mock.contractUpdateActivationHeight(submit))

	// Testnet: always timelocked at the short testnet duration (no mainnet gate).
	testnet := &StateEngine{sconf: systemconfig.TestnetConfig()}
	assert.Equal(t,
		submit+params.CONTRACT_UPDATE_TIMELOCK_BLOCKS_TESTNET,
		testnet.contractUpdateActivationHeight(submit),
	)

	// Mainnet: gated by ConsensusParams.Version0_2_0Height.

	// Gate unset (0 == disabled): updates stay immediate (today's behavior).
	mainnetUngated := &StateEngine{sconf: mainnetWithTimelockHeight(0)}
	assert.Equal(t, submit, mainnetUngated.contractUpdateActivationHeight(submit))

	// Gate pinned at `submit`.
	mainnetGated := &StateEngine{sconf: mainnetWithTimelockHeight(submit)}

	// Below the gate stays immediate (preserves historical replay).
	assert.Equal(t, submit-1, mainnetGated.contractUpdateActivationHeight(submit-1))

	// At/after the gate is timelocked by the full 48h block count.
	assert.Equal(t,
		submit+params.CONTRACT_UPDATE_TIMELOCK_BLOCKS,
		mainnetGated.contractUpdateActivationHeight(submit),
	)
}
