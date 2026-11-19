package election_proposer

import (
	"math/big"
	"testing"

	"vsc-node/lib/dids"
	"vsc-node/modules/db/vsc/elections"
)

func i25Election(keys []string, weights []uint64, protocol uint64) elections.ElectionResult {
	var r elections.ElectionResult
	for i, k := range keys {
		r.Members = append(r.Members, elections.ElectionMember{Key: k, Account: "m" + string(rune('a'+i))})
	}
	r.Weights = weights
	r.ProtocolVersion = protocol
	return r
}

func bits(idx ...int) *big.Int {
	b := new(big.Int)
	for _, i := range idx {
		b.SetBit(b, i, 1)
	}
	return b
}

// I-25, testnet 11-04 election 1479: 6 members, the 4 lowest-indexed signed.
// The proposer summed weight only up to the highest signer's bit and logged
// voted 4 of total 4; the verifiers total over all 6. It now folds exactly as
// the verifiers do (state-processing system_txs.go).
func TestI25_ProposerTotalsOverEveryMember(t *testing.T) {
	keys := []string{"k0", "k1", "k2", "k3", "k4", "k5"}
	for _, protocol := range []uint64{7, 8} { // before and after B13 (0.8 dedup)
		voted, total := electionVoteWeights(i25Election(keys, []uint64{1, 1, 1, 1, 1, 1}, protocol), bits(0, 1, 2, 3))
		if voted != 4 || total != 6 {
			t.Fatalf("protocol %d: voted %d of %d, want 4 of 6 (the old sum said 4 of 4)", protocol, voted, total)
		}
	}
	// a legacy election without weights: every member counts one
	if voted, total := electionVoteWeights(i25Election(keys, nil, 7), bits(0, 5)); voted != 2 || total != 6 {
		t.Fatalf("no weights: voted %d of %d, want 2 of 6", voted, total)
	}
	// B13 active: a duplicated BLS key counts once, in the signed and the total
	// weight, as dids.FoldSignedWeight folds it for the verifiers
	dup := []string{"k0", "k0", "k2", "k3"}
	voted, total := electionVoteWeights(i25Election(dup, []uint64{5, 5, 1, 1}, 8), bits(0, 1, 2))
	wantV, wantT, _ := dids.FoldSignedWeight(dup, []uint64{5, 5, 1, 1}, []dids.BlsDID{"k0", "k0", "k2"})
	if voted != wantV || total != wantT {
		t.Fatalf("dedup: voted %d of %d, want the verifiers' %d of %d", voted, total, wantV, wantT)
	}
}
