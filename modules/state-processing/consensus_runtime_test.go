package state_engine_test

import (
	"testing"

	"vsc-node/lib/test_utils"
)

func TestConsensusRuntime_LineFromElectionAndUnregisteredExecutor(t *testing.T) {
	mem := test_utils.NewMockConsensusState()
	te := newTestEnvWithConsensus(mem, nil)
	te.ElectionDb.ElectionsByHeight[1] = versionedElection(1, 1, 1, 2) // active 1.2
	te.processAndWait()

	line := te.SE.ActiveConsensusLine(1)
	if line.Major != 1 || line.Consensus != 2 {
		t.Fatalf("expected active line 1.2, got %d.%d", line.Major, line.Consensus)
	}
	if te.SE.ActiveConsensusExecutor(1).Name() != "passthrough-unregistered" {
		t.Fatalf("expected explicit unregistered executor marker for non-default line")
	}
}

func TestConsensusRuntime_ActiveLineIsPureFunctionOfHeight(t *testing.T) {
	mem := test_utils.NewMockConsensusState()
	te := newTestEnvWithConsensus(mem, nil)
	// Two elections at distinct heights with distinct versions; the active line at each height
	// must resolve from the on-chain election at that height (replay-correct, no live cache).
	te.ElectionDb.ElectionsByHeight[1] = versionedElection(1, 1, 1, 2)
	te.ElectionDb.ElectionsByHeight[20] = versionedElection(20, 2, 1, 3)
	te.processAndWait()

	if l := te.SE.ActiveConsensusLine(1); l.Major != 1 || l.Consensus != 2 {
		t.Fatalf("height 1 expected 1.2, got %d.%d", l.Major, l.Consensus)
	}
	if l := te.SE.ActiveConsensusLine(20); l.Major != 1 || l.Consensus != 3 {
		t.Fatalf("height 20 expected 1.3, got %d.%d", l.Major, l.Consensus)
	}
}
