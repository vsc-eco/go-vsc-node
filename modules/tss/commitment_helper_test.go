package tss

import "testing"

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
