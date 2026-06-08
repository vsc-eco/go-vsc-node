package state_engine

// review8 GV-H5 — vsc.recovery_suspend / vsc.recovery_require_version are gated
// by VerifyRecoveryMultisig, which returns false whenever the network's
// ConsensusParams omit RecoveryMultisigAccounts/Threshold. Mainnet omits them,
// so the on-chain emergency stop is permanently inert and the handler returned
// SILENTLY. This test pins the helper semantics (configured vs unauthorized vs
// unconfigured) and documents the live mainnet gap. The handler now logs the
// unconfigured case loudly (see executeRecoverySuspend); populating the roster
// is an operator/governance decision tracked separately.

import (
	"testing"

	"vsc-node/modules/common/params"
	"vsc-node/modules/common/system-config"

	"github.com/stretchr/testify/require"
)

func TestReview8_GVH5_RecoveryMultisigConfiguredSemantics(t *testing.T) {
	// Unconfigured: no roster at all -> inert (the mainnet reality / the bug).
	require.False(t, RecoveryMultisigConfigured(params.ConsensusParams{}),
		"empty params must report unconfigured")
	require.False(t, VerifyRecoveryMultisig(params.ConsensusParams{}, []string{"hive:a", "hive:b"}),
		"verify must fail when unconfigured regardless of signers")

	// Threshold present but no accounts, or accounts but zero threshold: still inert.
	require.False(t, RecoveryMultisigConfigured(params.ConsensusParams{RecoveryMultisigThreshold: 2}))
	require.False(t, RecoveryMultisigConfigured(params.ConsensusParams{RecoveryMultisigAccounts: []string{"a", "b"}}))

	// Threshold exceeds roster size: unsatisfiable -> inert.
	require.False(t, RecoveryMultisigConfigured(params.ConsensusParams{
		RecoveryMultisigAccounts:  []string{"a", "b"},
		RecoveryMultisigThreshold: 3,
	}))

	// Properly configured 2-of-3.
	cfg := params.ConsensusParams{
		RecoveryMultisigAccounts:  []string{"alice", "bob", "carol"},
		RecoveryMultisigThreshold: 2,
	}
	require.True(t, RecoveryMultisigConfigured(cfg))

	// Configured + enough distinct authorized signers -> authorized.
	require.True(t, VerifyRecoveryMultisig(cfg, []string{"hive:alice", "hive:bob"}),
		"two distinct roster signers meet the 2-of-3 threshold")
	// hive: prefix on the roster side is also tolerated.
	require.True(t, VerifyRecoveryMultisig(params.ConsensusParams{
		RecoveryMultisigAccounts:  []string{"hive:alice", "hive:bob", "hive:carol"},
		RecoveryMultisigThreshold: 2,
	}, []string{"alice", "carol"}))

	// Configured but unauthorized: too few distinct roster signers -> rejected,
	// and this is DISTINCT from unconfigured (RecoveryMultisigConfigured stays true).
	require.False(t, VerifyRecoveryMultisig(cfg, []string{"hive:alice"}),
		"one signer cannot meet a 2-of-3 threshold")
	require.False(t, VerifyRecoveryMultisig(cfg, []string{"hive:alice", "hive:alice"}),
		"duplicate signer counts once")
	require.False(t, VerifyRecoveryMultisig(cfg, []string{"hive:mallory", "hive:eve"}),
		"off-roster signers do not count")
	require.True(t, RecoveryMultisigConfigured(cfg),
		"a rejected attempt against a real roster is NOT the unconfigured case")
}

// TestReview8_GVH5_MainnetRecoveryInert documents the PROVEN finding: the shipped
// mainnet ConsensusParams leave the recovery multisig unconfigured, so the
// emergency stop is inert. When an operator finally populates the roster in
// MainnetConfig(), flip this assertion (and remove the skip) — the code path is
// already proven to work by the test above.
func TestReview8_GVH5_MainnetRecoveryInert(t *testing.T) {
	mainnet := systemconfig.MainnetConfig().ConsensusParams()
	if RecoveryMultisigConfigured(mainnet) {
		t.Log("recovery multisig is now configured on mainnet — GV-H5 config gap closed")
		return
	}
	require.False(t, RecoveryMultisigConfigured(mainnet),
		"GV-H5: mainnet recovery multisig is currently UNCONFIGURED (inert emergency stop) — operator must populate the roster")
}
