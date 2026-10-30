package state_engine

import (
	"errors"
	"testing"
	"time"

	"vsc-node/modules/common/consensusversion"
	"vsc-node/modules/db/vsc/elections"

	"go.mongodb.org/mongo-driver/mongo"
)

// flakyElectionDb returns a transient error for the first `failFor` calls, then
// succeeds. Used to prove ActiveConsensusVersion RETRIES rather than silently
// reporting 0.0.0 (which would turn every consensus gate off on this node alone).
type flakyElectionDb struct {
	elections.Elections
	failFor int
	calls   int
	result  elections.ElectionResult
	err     error // if set, returned every call (for the deterministic-absence case)
}

func (f *flakyElectionDb) GetElectionByHeight(height uint64) (elections.ElectionResult, error) {
	f.calls++
	if f.err != nil {
		return elections.ElectionResult{}, f.err
	}
	if f.calls <= f.failFor {
		return elections.ElectionResult{}, errors.New("connection reset by peer")
	}
	return f.result, nil
}

func versionResult(consensus uint64) elections.ElectionResult {
	var r elections.ElectionResult
	r.ProtocolVersion = consensus
	return r
}

// TestActiveConsensusVersion_RetriesTransientRead proves the fail-STOP half.
//
// Before this fix the method returned consensusversion.Version{} on ANY read error.
// That is "consensus 0.0.0", which turns off every gate resolved through it —
// including IsPoaExitHalted, which decides whether a consensus bond payout is HELD.
// A single Mongo blip on one node therefore released a bond its peers refused: a
// silent, one-node, fund-moving ledger divergence with no error surfaced anywhere.
func TestActiveConsensusVersion_RetriesTransientRead(t *testing.T) {
	db := &flakyElectionDb{failFor: 3, result: versionResult(5)}
	se := &StateEngine{electionDb: db}

	done := make(chan consensusversion.Version, 1)
	go func() { done <- se.ActiveConsensusVersion(100) }()

	select {
	case got := <-done:
		if got.Consensus != 5 {
			t.Fatalf("got consensus %d, want 5 — a transient read must NOT be reported as 0.0.0",
				got.Consensus)
		}
		if db.calls < 4 {
			t.Fatalf("only %d call(s): the transient error was not retried, so this test would "+
				"pass even with the old fail-open behaviour", db.calls)
		}
		t.Logf("retried %d times then returned consensus=%d", db.calls-1, got.Consensus)
	case <-time.After(30 * time.Second):
		t.Fatal("ActiveConsensusVersion never returned — retry loop did not converge")
	}
}

// TestActiveConsensusVersion_NoDocumentsIsNotRetried proves the other half, and it
// is the one that would take the whole network down if it were wrong.
//
// GetElectionByHeight queries block_height $lt height, so BEFORE the first election
// is processed every block legitimately has no election below it. If ErrNoDocuments
// were treated as transient, blockingRetry would spin forever and EVERY node would
// wedge at genesis, on every network. The exemption is load-bearing.
func TestActiveConsensusVersion_NoDocumentsIsNotRetried(t *testing.T) {
	db := &flakyElectionDb{err: mongo.ErrNoDocuments}
	se := &StateEngine{electionDb: db}

	done := make(chan consensusversion.Version, 1)
	go func() { done <- se.ActiveConsensusVersion(100) }()

	select {
	case got := <-done:
		if got != (consensusversion.Version{}) {
			t.Fatalf("got %v, want the zero Version — a deterministic absence is the correct "+
				"pre-genesis answer", got)
		}
		if db.calls != 1 {
			t.Fatalf("ErrNoDocuments was retried %d times. Retrying a DETERMINISTIC absence "+
				"wedges every node at genesis forever: before the first election exists, every "+
				"block hits this path.", db.calls)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ActiveConsensusVersion BLOCKED on mongo.ErrNoDocuments — this is the genesis " +
			"wedge: no node could ever sync past its start height")
	}
}

// TestDisplayConsensusVersion_DoesNotBlock proves the display path was excluded.
// DisplayConsensusVersion backs a GraphQL field; blocking an API handler on a DB
// blip is its own outage, and a display value is not a consensus decision.
func TestDisplayConsensusVersion_DoesNotBlock(t *testing.T) {
	db := &flakyElectionDb{err: errors.New("connection reset by peer")}
	se := &StateEngine{electionDb: db}

	done := make(chan consensusversion.Version, 1)
	go func() { done <- se.displayActiveConsensusVersion(100) }()

	select {
	case <-done:
		if db.calls != 1 {
			t.Errorf("display path retried %d times; it must stay non-blocking", db.calls)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("displayActiveConsensusVersion BLOCKED on a transient error — an API handler " +
			"must not hang on a DB blip")
	}
}
