package state_engine

import (
	"testing"

	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"

	"github.com/stretchr/testify/assert"
)

// TestContractUpdateActivationHeight covers the network-aware, consensus-gated
// timelock policy that decides when a queued update activates. The duration is
// network-baked (non-overridable) and, on mainnet, only applies at/after the
// pinned rollout height so historical reindex stays byte-identical.
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

	// Mainnet: gated by CONTRACT_UPDATE_TIMELOCK_HEIGHT.
	mainnet := &StateEngine{sconf: systemconfig.MainnetConfig()}
	orig := params.CONTRACT_UPDATE_TIMELOCK_HEIGHT
	defer func() { params.CONTRACT_UPDATE_TIMELOCK_HEIGHT = orig }()

	// Gate unset (0 == disabled): updates stay immediate (today's behavior).
	params.CONTRACT_UPDATE_TIMELOCK_HEIGHT = 0
	assert.Equal(t, submit, mainnet.contractUpdateActivationHeight(submit))

	// Gate pinned: below the gate stays immediate (preserves historical replay).
	params.CONTRACT_UPDATE_TIMELOCK_HEIGHT = submit
	assert.Equal(t, submit-1, mainnet.contractUpdateActivationHeight(submit-1))

	// Gate pinned: at/after the gate is timelocked by the full 48h block count.
	assert.Equal(t,
		submit+params.CONTRACT_UPDATE_TIMELOCK_BLOCKS,
		mainnet.contractUpdateActivationHeight(submit),
	)
}
