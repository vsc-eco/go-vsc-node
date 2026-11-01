package election_proposer

import (
	"testing"

	"vsc-node/modules/common"
)

// Mainnet epoch 2210: epoch 2209 landed at Hive 111222854, ElectionInterval
// 7200. The canonical anchor 111230100 saw 10 of 16 witnesses announced
// enabled=false (fleet restart onto the new image right before it), built a
// 6-member committee against MinMembers 8, and every retry rebuilt the same
// election at the same anchor.
const (
	stuckPrevBh    = uint64(111222854)
	stuckInterval  = uint64(7200)
	stuckCanonical = uint64(111230100)
)

func TestElectionAnchorAtMainnetStall(t *testing.T) {
	if got := canonicalElectionAnchor(stuckPrevBh, stuckInterval); got != stuckCanonical {
		t.Fatalf("canonical anchor = %d, want %d (the anchor in the mainnet logs)", got, stuckCanonical)
	}
	cases := []struct {
		name string
		bh   uint64
		want uint64
	}{
		{"first trigger", stuckCanonical, stuckCanonical},
		{"first retry seen in the logs", 111232050, stuckCanonical},
		{"last block of the first interval", stuckCanonical + stuckInterval - 1, stuckCanonical},
		{"one interval overdue", stuckCanonical + stuckInterval, stuckCanonical + stuckInterval},
		{"inside the second interval", stuckCanonical + stuckInterval + 49, stuckCanonical + stuckInterval},
		{"last retry seen in the logs", 111279700, stuckCanonical + 6*stuckInterval},
		{"signer behind the anchor", stuckCanonical - 10, stuckCanonical},
		{"signer far behind", 100, stuckCanonical},
	}
	for _, c := range cases {
		if got := electionAnchorAt(stuckPrevBh, stuckInterval, c.bh); got != c.want {
			t.Errorf("%s: electionAnchorAt(bh=%d) = %d, want %d", c.name, c.bh, got, c.want)
		}
	}
}

// A healthy epoch must see exactly today's anchor: for every height within the
// first interval after the canonical anchor (and every height before it), the
// stepped anchor equals the canonical one.
func TestElectionAnchorAtUnchangedWhileNotOverdue(t *testing.T) {
	for _, interval := range []uint64{20, 40, 1000, 3600, 7200} {
		for _, prev := range []uint64{0, 1, 49, 50, 1234, 111222854} {
			canonical := canonicalElectionAnchor(prev, interval)
			for bh := prev; bh < canonical+interval; bh++ {
				if got := electionAnchorAt(prev, interval, bh); got != canonical {
					t.Fatalf("interval %d prev %d bh %d: anchor %d, want canonical %d", interval, prev, bh, got, canonical)
				}
			}
		}
	}
}

// The proposer derives the anchor at its trigger block T (a cadence multiple);
// a signer handles the sign_request a few blocks later. Every height in
// [T, T+cadence) must derive the proposer's anchor, and the anchor must never
// pass the height it was derived at (GenerateElectionAtBlock waits for it).
func TestElectionAnchorAtProposerSignerAgree(t *testing.T) {
	cadence := 5 * common.CONSENSUS_SPECS.SlotLength
	for _, interval := range []uint64{20, 40, 1000, 7200} {
		prev := uint64(111222854)
		canonical := canonicalElectionAnchor(prev, interval)
		last := canonical
		for T := canonical; T <= canonical+12*interval+cadence; T += cadence {
			want := electionAnchorAt(prev, interval, T)
			if want > T {
				t.Fatalf("interval %d T %d: anchor %d is above the trigger", interval, T, want)
			}
			if want < last {
				t.Fatalf("interval %d T %d: anchor went backwards %d -> %d", interval, T, last, want)
			}
			if (want-canonical)%interval != 0 {
				t.Fatalf("interval %d T %d: anchor %d is off the canonical+k*interval grid", interval, T, want)
			}
			last = want
			for s := T; s < T+cadence; s++ {
				if got := electionAnchorAt(prev, interval, s); got != want {
					t.Fatalf("interval %d: proposer at %d derives %d, signer at %d derives %d", interval, T, want, s, got)
				}
			}
		}
	}
}

func TestElectionAnchorAtZeroInterval(t *testing.T) {
	if got, want := electionAnchorAt(1000, 0, 1_000_000), canonicalElectionAnchor(1000, 0); got != want {
		t.Fatalf("interval 0: got %d, want canonical %d", got, want)
	}
}
