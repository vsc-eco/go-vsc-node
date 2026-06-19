package state_engine

import (
	"testing"

	"vsc-node/modules/common/consensusversion"
	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"

	"github.com/stretchr/testify/assert"
)

// TestContractUpdateActivationHeight covers the network-aware, consensus-gated
// timelock policy that decides when a queued update activates. The duration is
// network-baked (non-overridable) and, on mainnet, only applies once the
// chain-active consensus version has reached 0.2.0 so historical reindex stays
// byte-identical. contractUpdateActivationHeight takes the active version
// directly (resolved by the caller from se.ActiveConsensusVersion), so the
// policy is a pure function we can drive without an election DB.
func TestContractUpdateActivationHeight(t *testing.T) {
	const submit = uint64(200_000_000)

	v010 := consensusversion.Version{Major: 0, Consensus: 1} // below the v0.2.0 line
	v020 := consensusversion.V0_2_0                          // at the v0.2.0 line

	// Mocknet: timelock disabled (0 blocks) -> immediate (version irrelevant).
	mock := &StateEngine{sconf: systemconfig.MocknetConfig()}
	assert.Equal(t, submit, mock.contractUpdateActivationHeight(submit, v020))

	// Testnet: always timelocked at the short testnet duration (no mainnet gate;
	// version irrelevant off-mainnet).
	testnet := &StateEngine{sconf: systemconfig.TestnetConfig()}
	assert.Equal(t,
		submit+params.CONTRACT_UPDATE_TIMELOCK_BLOCKS_TESTNET,
		testnet.contractUpdateActivationHeight(submit, v010),
	)

	// Mainnet: gated on the chain-active consensus version reaching 0.2.0.
	mainnet := &StateEngine{sconf: systemconfig.MainnetConfig()}

	// Below 0.2.0: updates stay immediate (preserves historical replay).
	assert.Equal(t, submit, mainnet.contractUpdateActivationHeight(submit, v010))

	// At/after 0.2.0: timelocked by the full 48h block count.
	assert.Equal(t,
		submit+params.CONTRACT_UPDATE_TIMELOCK_BLOCKS,
		mainnet.contractUpdateActivationHeight(submit, v020),
	)
}
