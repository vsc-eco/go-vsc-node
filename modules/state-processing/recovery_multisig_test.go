package state_engine

import (
	"testing"

	"vsc-node/modules/common/params"
)

func TestVerifyRecoveryMultisig(t *testing.T) {
	p := params.ConsensusParams{
		RecoveryMultisigAccounts: []string{"alice", "bob", "carol"},
		RecoveryMultisigThreshold: 2,
	}
	if VerifyRecoveryMultisig(p, []string{"hive:alice"}) {
		t.Fatal("expected false for single signer")
	}
	if !VerifyRecoveryMultisig(p, []string{"hive:alice", "hive:bob"}) {
		t.Fatal("expected true for two signers")
	}
	if !VerifyRecoveryMultisig(p, []string{"alice", "carol"}) {
		t.Fatal("expected true without hive prefix")
	}
	if VerifyRecoveryMultisig(params.ConsensusParams{}, []string{"hive:alice"}) {
		t.Fatal("empty config must reject")
	}
}
