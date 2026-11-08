package state_engine

import (
	"fmt"
	"testing"

	"vsc-node/modules/db/vsc/elections"
	vscBlocks "vsc-node/modules/db/vsc/vsc_blocks"
	"vsc-node/modules/incentive-pendulum/rewards"
)

// poaElection builds the live mainnet shape: n POA seats, flat weight 1, one
// distinct BLS key each.
func poaElection(n int) elections.ElectionResult {
	e := elections.ElectionResult{}
	for i := 0; i < n; i++ {
		e.Members = append(e.Members, elections.ElectionMember{
			Account: fmt.Sprintf("w%02d", i),
			Key:     fmt.Sprintf("did:key:z%02d", i),
		})
		e.Weights = append(e.Weights, 1)
	}
	e.TotalWeight = uint64(n)
	return e
}

// TestAttestationQuorumFor_FlatPoaCommittee: the 16-seat committee folds to
// 16/16 with one unit per account, which is what makes the leader's bar read as
// 14 downstream (REWARD-ATTEST-1).
func TestAttestationQuorumFor_FlatPoaCommittee(t *testing.T) {
	q := attestationQuorumFor(poaElection(16))
	if q == nil {
		t.Fatal("expected a quorum for a well-formed 16-seat election")
	}
	if q.FoldedTotal != 16 || q.ElectionTotal != 16 {
		t.Fatalf("folded=%d election=%d, want 16/16", q.FoldedTotal, q.ElectionTotal)
	}
	if len(q.WeightByAccount) != 16 {
		t.Fatalf("got %d accounts, want 16", len(q.WeightByAccount))
	}
	for acct, w := range q.WeightByAccount {
		if w != 1 {
			t.Errorf("%s weighs %d, want 1", acct, w)
		}
	}
}

// TestAttestationQuorumFor_DuplicateKeyCollapses: two seats under one BLS key
// cannot sign independently, so the fold has to give the second seat 0 and drop
// it from the total — exactly as dids.FoldSignedWeight does. Left in, a
// duplicated key would raise the signed weight of every block it appears in
// (the header names BOTH accounts for one signature) and could make a block
// look closed when the real signers never reached the bar.
func TestAttestationQuorumFor_DuplicateKeyCollapses(t *testing.T) {
	e := poaElection(16)
	e.Members[15].Key = e.Members[0].Key // w15 free-rides on w00's key

	q := attestationQuorumFor(e)
	if q == nil {
		t.Fatal("expected a quorum")
	}
	if q.FoldedTotal != 15 {
		t.Errorf("folded total = %d, want 15 (the duplicate seat carries none)", q.FoldedTotal)
	}
	if q.ElectionTotal != 16 {
		t.Errorf("election total = %d, want the raw stored 16 (the value the leader compares against)", q.ElectionTotal)
	}
	if q.WeightByAccount["w15"] != 0 {
		t.Errorf("duplicate seat w15 weighs %d, want 0", q.WeightByAccount["w15"])
	}
	if q.WeightByAccount["w00"] != 1 {
		t.Errorf("first holder w00 weighs %d, want 1", q.WeightByAccount["w00"])
	}
}

// TestAttestationQuorumFor_LegacyElectionWithoutWeights: pre-weight elections
// store no weight vector and count one per member, the same reading
// elections.CalculateSigningScore and StoreElection give them. Returning nil
// here instead would leave those epochs on the strict rule for no reason.
func TestAttestationQuorumFor_LegacyElectionWithoutWeights(t *testing.T) {
	e := poaElection(9)
	e.Weights = nil

	q := attestationQuorumFor(e)
	if q == nil {
		t.Fatal("a legacy weightless election must still fold, one unit per member")
	}
	if q.FoldedTotal != 9 {
		t.Errorf("folded total = %d, want 9", q.FoldedTotal)
	}
	for acct, w := range q.WeightByAccount {
		if w != 1 {
			t.Errorf("%s weighs %d, want 1", acct, w)
		}
	}
}

// TestAttestationQuorumFor_RefusesWhatItCannotWeigh: a weight vector that
// exists but does not line up with the members is corrupt, not legacy, and an
// empty election has nothing to fold. Both must return nil, which leaves
// attestation scoring on the strict pre-0.14.0 rule. Fail-open here — treating
// an unknown bar as met — would stop docking network-wide on malformed state,
// and an overpayment cannot be clawed back.
func TestAttestationQuorumFor_RefusesWhatItCannotWeigh(t *testing.T) {
	short := poaElection(16)
	short.Weights = short.Weights[:10]
	if q := attestationQuorumFor(short); q != nil {
		t.Errorf("a 16-member election with 10 weights must not fold, got %+v", q)
	}
	if q := attestationQuorumFor(elections.ElectionResult{}); q != nil {
		t.Errorf("an empty election must not fold, got %+v", q)
	}
}

// TestAttestationQuorumFor_MatchesTheScorerOnTheLiveShape closes the loop
// between the two halves of the fix: the fold the state engine builds from a
// stored election, fed to the scorer, must reproduce the 14-of-16 bar. Each
// half passing its own unit test would not prove the pair agrees.
func TestAttestationQuorumFor_MatchesTheScorerOnTheLiveShape(t *testing.T) {
	e := poaElection(16)
	committee := make([]string, 0, len(e.Members))
	for _, m := range e.Members {
		committee = append(committee, m.Account)
	}
	q := attestationQuorumFor(e)

	for _, tc := range []struct {
		signers int
		wantBps int
	}{
		{16, 0},
		{15, 0},
		{14, 0},                               // the leader's stop point
		{13, rewards.BlockAttestationMissBps}, // a timed-out collection window
		{11, rewards.BlockAttestationMissBps},
	} {
		got := rewards.ScoreBlockAttestation(
			[]vscBlocks.VscHeaderRecord{{Signers: committee[:tc.signers]}}, committee, q,
		)
		for _, absent := range committee[tc.signers:] {
			if got[absent] != tc.wantBps {
				t.Errorf("%d of 16 signed: absent seat %s got %d bps, want %d",
					tc.signers, absent, got[absent], tc.wantBps)
			}
		}
		for _, signer := range committee[:tc.signers] {
			if got[signer] != 0 {
				t.Errorf("%d of 16 signed: seat %s signed and must owe nothing, got %d bps",
					tc.signers, signer, got[signer])
			}
		}
	}
}
