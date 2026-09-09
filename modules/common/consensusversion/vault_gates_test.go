package consensusversion

import "testing"

// The vault-rotation-v2 line must sit above every line already claimed on any
// branch. 0.4.0/0.5.0 are develop's delegated-consensus-stake batch, 0.6.0 is
// feat/vault-protection's, and 0.7.0 is the POA admission batch. Reusing a taken
// line would make one floor rise activate two unrelated batches at once, which is
// precisely the coordinated-activation property the version floor provides.
func TestVaultRotationV2LineIsAboveEveryClaimedLine(t *testing.T) {
	if V0_8_0.Consensus <= V0_7_0.Consensus {
		t.Fatalf("vault-v2 line consensus=%d must be > POA's %d", V0_8_0.Consensus, V0_7_0.Consensus)
	}
	if V0_8_0.Consensus <= 6 {
		t.Fatalf("vault-v2 line consensus=%d collides with a claimed line (4/5 delegated stake, 6 vault-protection)",
			V0_8_0.Consensus)
	}
}

// The shipped binary must announce EXACTLY the highest batch it implements.
//
// Too low and the floor can never rise to activate the batch: a witness announcing
// below the pinned floor is deleted from the committee (election-proposer.go's
// PinnedVersionFloor filter), so a floor rise to 0.8.0 while binaries still
// announce 0.7.0 would empty the committee. Too high and the binary claims code it
// does not carry, letting the floor rise past what it can actually execute.
//
// This is the ONE place to update when a new consensus line ships: bump
// currentConsensus in version.go and this assertion in the same commit.
func TestRunningVersionImplementsHighestBatch(t *testing.T) {
	if RunningVersion().Cmp(V0_8_0) != 0 {
		t.Errorf("RunningVersion() = %s, want %s (bump currentConsensus in version.go when shipping a new line)",
			RunningVersion().Format(), V0_8_0.Format())
	}
}

// Below the line every v2 rule is inert, which is what lets old and new binaries
// interoperate through the rollout. Above it they are all in force together.
func TestVaultRotationV2ActiveOnlyAtOrAboveTheLine(t *testing.T) {
	for _, tc := range []struct {
		active Version
		want   bool
	}{
		{Version{}, false},                                  // no election stored yet (fresh genesis)
		{Version{Major: 0, Consensus: 3}, false},            // mainnet today
		{Version{Major: 0, Consensus: 7}, false},            // POA shipped, vault-v2 not yet
		{Version{Major: 0, Consensus: 8}, true},             // the line itself
		{Version{Major: 0, Consensus: 9}, true},             // a later line still carries it
		// A major bump does NOT imply the batch: MeetsConsensusMin compares the
		// components independently (Major >= min.Major AND Consensus >= min.Consensus),
		// so 1.0.0 does not satisfy a 0.8.0 minimum. That is the semantic every gate in
		// this package already has; pinned here so a future change to the comparison is
		// caught rather than silently deactivating a shipped batch.
		{Version{Major: 1, Consensus: 0}, false},
		{Version{Major: 1, Consensus: 8}, true},
	} {
		if got := VaultRotationV2Active(tc.active); got != tc.want {
			t.Errorf("VaultRotationV2Active(%s) = %v, want %v", tc.active.Format(), got, tc.want)
		}
	}
}
