package rewards

import (
	"fmt"
	"testing"
)

// flatQuorum builds the AttestationQuorum for a POA committee: n seats, one
// weight each, no duplicate keys — the live mainnet shape.
func flatQuorum(committee []string) *AttestationQuorum {
	byAccount := make(map[string]uint64, len(committee))
	for _, a := range committee {
		byAccount[a] = 1
	}
	return &AttestationQuorum{
		WeightByAccount: byAccount,
		FoldedTotal:     uint64(len(committee)),
		ElectionTotal:   uint64(len(committee)),
	}
}

func seats(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf("w%02d", i))
	}
	return out
}

// signersExcept returns the committee minus the named seats.
func signersExcept(committee []string, absent ...string) []string {
	skip := make(map[string]struct{}, len(absent))
	for _, a := range absent {
		skip[a] = struct{}{}
	}
	out := make([]string, 0, len(committee))
	for _, m := range committee {
		if _, drop := skip[m]; drop {
			continue
		}
		out = append(out, m)
	}
	return out
}

// TestAttestationQuorum_LiveMainnetShape reproduces REWARD-ATTEST-1 at the
// shape it is live at: 16 seats, POA flat weight 1, and a leader that stops
// collecting at 14 (dids.LeaderSigsComplete: 16*9/10 == 14, and 14 > 16*2/3).
// Two seats are therefore missing from the Signers list of every healthy block,
// and which two is decided by whose signature arrived last.
//
// The CONTROL is the same fixture with quorum nil — the rule below the 0.14.0
// line. It must still dock all 16 seats, or this test proves nothing about the
// fix: a scorer that returned nothing for an unrelated reason would pass the
// fixed case on its own.
func TestAttestationQuorum_LiveMainnetShape(t *testing.T) {
	committee := seats(16)

	// Eight blocks, each closed by the leader at 14 of 16 signatures, the
	// absent pair rotating so every seat is last exactly once — the pattern
	// that charged every mainnet seat in epoch 2221, none of them at zero.
	blocks := make([]TickBlockHeader, 0, 8)
	for i := 0; i < 8; i++ {
		blocks = append(blocks, TickBlockHeader{
			Signers: signersExcept(committee, committee[2*i], committee[2*i+1]),
		})
	}
	for i, b := range blocks {
		if len(b.Signers) != 14 {
			t.Fatalf("fixture block %d has %d signers, want 14 (the bar the leader stops at)", i, len(b.Signers))
		}
	}

	// CONTROL: below the line, every one of the 16 honest seats is docked.
	control := scoreAttestationFromHeaders(blocks, committee, nil)
	for _, m := range committee {
		if control[m] != BlockAttestationMissBps {
			t.Fatalf("control (quorum nil): %s got %d bps, want %d — the pre-fix rule must dock, or the fixed case proves nothing",
				m, control[m], BlockAttestationMissBps)
		}
	}

	// FIXED: every block closed at the bar, so no absence is evidence.
	got := scoreAttestationFromHeaders(blocks, committee, flatQuorum(committee))
	for _, m := range committee {
		if got[m] != 0 {
			t.Errorf("%s got %d bps, want 0: the leader never waited past 14 signatures, so its absence from a 14-signer block says nothing", m, got[m])
		}
	}
}

// TestAttestationQuorum_BelowTheBarStillScores is the other half of the rule:
// a block broadcast BELOW the leader's bar means the collection window timed
// out — the leader did wait, and the signatures did not come. Those absences
// are real and must still cost. Without this the fix would be indistinguishable
// from deleting the signal.
func TestAttestationQuorum_BelowTheBarStillScores(t *testing.T) {
	committee := seats(16)
	quorum := flatQuorum(committee)

	for _, tc := range []struct {
		name    string
		absent  []string
		wantBps int
	}{
		{"14 of 16 — the leader's stop point, nothing is owed", committee[:2], 0},
		{"15 of 16 — above the bar", committee[:1], 0},
		{"16 of 16 — nobody absent", nil, 0},
		{"13 of 16 — below the bar, the leader timed out", committee[:3], BlockAttestationMissBps},
		{"11 of 16 — barely past the block's own 2/3 check", committee[:5], BlockAttestationMissBps},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := []TickBlockHeader{{Signers: signersExcept(committee, tc.absent...)}}
			got := scoreAttestationFromHeaders(blocks, committee, quorum)
			for _, m := range tc.absent {
				if got[m] != tc.wantBps {
					t.Errorf("absent seat %s: got %d bps, want %d", m, got[m], tc.wantBps)
				}
			}
			for _, m := range signersExcept(committee, tc.absent...) {
				if got[m] != 0 {
					t.Errorf("seat %s signed the block and must owe nothing, got %d bps", m, got[m])
				}
			}
		})
	}
}

// TestAttestationQuorum_DuplicateKeySeatCannotCloseABlock: a seat whose BLS key
// duplicates another's needs no private key to be credited — a header's Signers
// list names both accounts for one signature (TxProposeBlock.ValidateDetailed
// maps each included DID to every member holding it). If the duplicate carried
// weight here, 13 distinct signers plus the free-rider would look like the 14
// the leader waits for, and the 2 genuinely-absent seats would escape scoring.
// The fold gives the duplicate 0 and drops it from the total, so the block
// reads as what it is: below the bar.
func TestAttestationQuorum_DuplicateKeySeatCannotCloseABlock(t *testing.T) {
	committee := seats(16)
	// w15 duplicates w00's key: 15 distinct keys, folded total 15.
	q := flatQuorum(committee)
	q.WeightByAccount["w15"] = 0
	q.FoldedTotal = 15

	// w00 signs, so the Signers list carries BOTH w00 and the duplicate w15.
	// Distinct signers: 13 (w00..w12). Bar: 15*9/10 = 13, and 13 > 16*2/3 = 10.
	signers := append(signersExcept(committee, "w13", "w14", "w15"), "w15")
	if len(signers) != 14 {
		t.Fatalf("fixture carries %d signer names, want 14 (13 real + the duplicate)", len(signers))
	}
	blocks := []TickBlockHeader{{Signers: signers}}

	got := scoreAttestationFromHeaders(blocks, committee, q)
	// The folded weight (13) clears the folded bar of 13, so this block DID
	// close: the duplicate neither helped nor hurt, because it weighs nothing.
	// What matters is that the result is driven by the 13 real signatures, not
	// by 14 names.
	for _, m := range []string{"w13", "w14"} {
		if got[m] != 0 {
			t.Errorf("%s: got %d, want 0 — 13 real signatures already met the folded bar", m, got[m])
		}
	}

	// Now take one real signature away: 12 distinct signers against a folded
	// bar of 13. The duplicate still makes 13 NAMES, and if it counted the
	// block would look closed. It must not.
	signers = append(signersExcept(committee, "w12", "w13", "w14", "w15"), "w15")
	got = scoreAttestationFromHeaders([]TickBlockHeader{{Signers: signers}}, committee, q)
	for _, m := range []string{"w12", "w13", "w14"} {
		if got[m] != BlockAttestationMissBps {
			t.Errorf("%s: got %d, want %d — a duplicated key must not make 12 signatures look like 13",
				m, got[m], BlockAttestationMissBps)
		}
	}
}

// TestAttestationQuorum_UnweighableElectionFallsBackToStrict: a quorum the
// state engine could not build (nil) or one with no foldable weight must leave
// the scorer on the pre-fix rule. Treating an unknown bar as "met" would stop
// docking network-wide on a DB hiccup, which is the one direction that loses
// money irrecoverably.
func TestAttestationQuorum_UnweighableElectionFallsBackToStrict(t *testing.T) {
	committee := seats(16)
	blocks := []TickBlockHeader{{Signers: signersExcept(committee, committee[:4]...)}}

	for name, q := range map[string]*AttestationQuorum{
		"nil quorum":   nil,
		"zero total":   {WeightByAccount: map[string]uint64{}, FoldedTotal: 0, ElectionTotal: 16},
		"empty weight": {WeightByAccount: nil, FoldedTotal: 16, ElectionTotal: 16},
	} {
		got := scoreAttestationFromHeaders(blocks, committee, q)
		for _, m := range committee[:4] {
			if got[m] != BlockAttestationMissBps {
				t.Errorf("%s: %s got %d bps, want %d (strict fallback)", name, m, got[m], BlockAttestationMissBps)
			}
		}
	}
}

// TestAggregateTick_AttestationQuorumReachesTheScorer pins the plumbing: the
// quorum has to travel on TickInputs and land in the consolidated Bps, not just
// work when scoreAttestation is called directly.
func TestAggregateTick_AttestationQuorumReachesTheScorer(t *testing.T) {
	committee := seats(16)
	blocks := []TickBlockHeader{
		{Signers: signersExcept(committee, "w00", "w01")}, // closed at 14
		{Signers: signersExcept(committee, "w00", "w01")},
	}

	withoutGate := AggregateTick(TickInputs{Committee: committee, BlocksInWindow: blocks})
	withGate := AggregateTick(TickInputs{
		Committee:         committee,
		BlocksInWindow:    blocks,
		AttestationQuorum: flatQuorum(committee),
	})

	find := func(recs []WitnessRewardReductionRecord, w string) WitnessRewardReductionRecord {
		for _, r := range recs {
			if r.Witness == w {
				return r
			}
		}
		t.Fatalf("no record for %s", w)
		return WitnessRewardReductionRecord{}
	}

	if r := find(withoutGate, "w00"); r.Evidence.BlockAttestationBps != 2*BlockAttestationMissBps || r.Bps != 2*BlockAttestationMissBps {
		t.Fatalf("control (below the line): w00 evidence=%d bps=%d, want %d for both",
			r.Evidence.BlockAttestationBps, r.Bps, 2*BlockAttestationMissBps)
	}
	if r := find(withGate, "w00"); r.Evidence.BlockAttestationBps != 0 || r.Bps != 0 {
		t.Fatalf("w00 evidence=%d bps=%d, want 0 for both", r.Evidence.BlockAttestationBps, r.Bps)
	}
}
