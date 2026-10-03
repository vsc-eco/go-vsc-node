package state_engine_test

import "testing"

// TestIsBondLockedRetiringMember_InertWhenFlagOff — #11 bond-lock is byte-identical
// INERT on every network today (VaultRotationV2ActivationHeight=0): the gate returns
// false immediately, before touching contract state / commitments / elections, so a
// consensus_unstake is never blocked. This is the load-bearing safety property (the
// functional locked→reject path is exercised on devnet, like the rest of the spine).
func TestIsBondLockedRetiringMember_InertWhenFlagOff(t *testing.T) {
	env := newTestEnv() // MocknetConfig — vault-rotation-v2 inert (activation height 0)
	for _, h := range []uint64{0, 1, 1000, 1 << 30} {
		if env.SE.IsBondLockedRetiringMember("hive:alice", h) {
			t.Fatalf("bond-lock must be inert (false) while vault-rotation-v2 is off, height=%d", h)
		}
	}
	if env.SE.IsBondLockedRetiringMember("", 1000) {
		t.Fatal("empty account must not be bond-locked")
	}
}
