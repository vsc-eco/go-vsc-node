package tss

import (
	"testing"

	tss_helpers "vsc-node/modules/tss/helpers"
)

// mustSetToCommitment adapts setToCommitment's (string, error) signature for
// tests whose fixture election is always present.
//
// setToCommitment started returning an error as part of the 18h fix: it used to
// call GetElection, which swallows read errors to a nil *ElectionResult, and
// then dereference it. Tests that only care about the encoding keep reading
// cleanly through this helper, while a fixture that stops providing the
// election now fails loudly instead of silently encoding an empty bitset.
func mustSetToCommitment(t *testing.T, mgr *TssManager, participants []Participant, epoch uint64) string {
	t.Helper()
	commitment, err := mgr.setToCommitment(participants, epoch)
	if err != nil {
		t.Fatalf("setToCommitment(epoch=%d) failed: %v", epoch, err)
	}
	return commitment
}

// mustAccusedCounts adapts reshareAccusedCounts' (map, error) signature for
// tests whose fixtures always read cleanly. It started returning an error as
// part of the H13 fail-closed fix: an incomplete accusation read used to fall
// through and count over a partial row set, which silently under-accuses.
func mustAccusedCounts(t *testing.T, mgr *TssManager, keyId string, bh, lastCommitHeight, expireBlock uint64) map[string]int {
	t.Helper()
	counts, err := mgr.reshareAccusedCounts(keyId, bh, lastCommitHeight, expireBlock)
	if err != nil {
		t.Fatalf("reshareAccusedCounts(%s) failed: %v", keyId, err)
	}
	return counts
}

// mustSerialize adapts DispatcherResult.Serialize's (BaseCommitment, error)
// signature for tests whose fixture election is always present.
//
// Serialize started returning an error as part of the 18h fix: the two blame
// shapes compute their bitset from the epoch's election, and a commitment built
// from a missing election is silently wrong rather than merely absent, so those
// paths must be able to decline to publish.
func mustSerialize(t *testing.T, r DispatcherResult) tss_helpers.BaseCommitment {
	t.Helper()
	c, err := r.Serialize()
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	return c
}
