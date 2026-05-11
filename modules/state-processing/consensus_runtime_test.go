package state_engine_test

import (
	"testing"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/common/consensusversion"
	"vsc-node/modules/db/vsc/consensus_state"
)

func TestConsensusRuntime_DefaultExecutorAndLine(t *testing.T) {
	mem := test_utils.NewMockConsensusState()
	mem.ReplaceState(consensus_state.ChainConsensusState{
		ID:             "singleton",
		AdoptedVersion: consensusversion.Version{Major: 1, Consensus: 2, NonConsensus: 9},
	})
	te := newTestEnvWithConsensus(mem, nil)
	te.processAndWait()

	line := te.SE.ActiveConsensusLine(1)
	if line.Major != 1 || line.Consensus != 2 {
		t.Fatalf("expected active line 1.2, got %d.%d", line.Major, line.Consensus)
	}
	if te.SE.ActiveConsensusExecutor(1).Name() != "passthrough-unregistered" {
		t.Fatalf("expected explicit unregistered executor marker for non-default line")
	}
}

func TestConsensusRuntime_ActivationHeightSwitch(t *testing.T) {
	mem := test_utils.NewMockConsensusState()
	mem.ReplaceState(consensus_state.ChainConsensusState{
		ID:             "singleton",
		AdoptedVersion: consensusversion.Version{Major: 1, Consensus: 2, NonConsensus: 0},
		NextActivation: &consensus_state.ConsensusActivation{
			Mode:                "normal",
			Version:             consensusversion.Version{Major: 1, Consensus: 3, NonConsensus: 0},
			ActivationHeight:    20,
			AttestedBlockHeight: 10,
			AttestedTxId:        "tx-activation",
		},
	})
	te := newTestEnvWithConsensus(mem, nil)
	te.processAndWait()

	before := te.SE.ActiveConsensusLine(19)
	if before.Major != 1 || before.Consensus != 2 {
		t.Fatalf("before activation expected 1.2, got %d.%d", before.Major, before.Consensus)
	}
	after := te.SE.ActiveConsensusLine(20)
	if after.Major != 1 || after.Consensus != 3 {
		t.Fatalf("at activation expected 1.3, got %d.%d", after.Major, after.Consensus)
	}
	a := te.SE.ConsensusActivation()
	if a == nil || a.Mode != "normal" || a.AttestedTxId != "tx-activation" {
		t.Fatalf("expected activation metadata copy from cache")
	}
	if te.SE.ActiveConsensusExecutor(20).Name() != "passthrough-unregistered" {
		t.Fatalf("expected unregistered executor marker at switched line")
	}
}
