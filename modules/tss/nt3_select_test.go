package tss

import "testing"

// TestSelectSignBroadcasts guards NT-3's never-brick rule: the leader broadcasts
// every sign result it holds, but a non-leader broadcasts ONLY results whose
// session party does not contain the leader, so a sign whose party has no leader
// (a fully churned-out retiring/drain committee) is still published rather than
// stranded. A leader-only policy (if the selection were just `isLeader`) returns
// nothing for the non-leader case and fails the second assertion.
func TestSelectSignBroadcasts(t *testing.T) {
	in := []signedSignResult{
		{res: KeySignResult{KeyId: "leader-in"}, leaderInParty: true},
		{res: KeySignResult{KeyId: "leader-out"}, leaderInParty: false},
	}

	// Leader publishes everything it holds.
	if got := selectSignBroadcasts(in, true); len(got) != 2 {
		t.Fatalf("leader should broadcast all %d results, got %d", len(in), len(got))
	}

	// Non-leader publishes ONLY the leader-absent session (never-brick); a
	// leader-only policy would return 0 here and fail.
	got := selectSignBroadcasts(in, false)
	if len(got) != 1 || got[0].KeyId != "leader-out" {
		t.Fatalf("non-leader should broadcast only the leader-absent result, got %+v", got)
	}

	// Degenerate: nothing in, nothing out, for either role.
	if got := selectSignBroadcasts(nil, true); len(got) != 0 {
		t.Fatalf("empty input should yield no broadcasts, got %d", len(got))
	}
}
