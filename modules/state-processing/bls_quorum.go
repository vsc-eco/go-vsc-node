package state_engine

import (
	"vsc-node/lib/dids"
	"vsc-node/modules/db/vsc/elections"
)

// BlsQuorumMet reports whether the set of BLS-included signer DIDs carries at
// least 2/3 of the total election weight.
//
// review2 CRITICAL #6: the state-engine previously accepted any
// cryptographically-valid aggregate signature without checking how much
// weight actually signed, so a sub-quorum commitment (e.g. 3 of 6 equal
// members, below the 2/3 threshold of 4 — observed for epochs 444-486) was
// accepted and could activate a TSS key / land a commitment. This predicate
// mirrors exactly the rule the leader enforces while collecting signatures in
// tss.go waitForSigs: `signedWeight*3 >= weightTotal*2`, with weights taken
// from the on-chain election (deterministic across nodes).
//
// Fail-closed: malformed election input, length mismatch, or zero total
// weight returns false. Unknown DIDs contribute zero and duplicates are
// counted once, so a forged/inflated bitset cannot manufacture quorum.
func BlsQuorumMet(included []dids.BlsDID, members []elections.ElectionMember, weights []uint64) bool {
	if len(members) == 0 || len(members) != len(weights) {
		return false
	}

	weightByDID := make(map[dids.BlsDID]uint64, len(members))
	var weightTotal uint64
	for i, m := range members {
		weightByDID[dids.BlsDID(m.Key)] = weights[i]
		weightTotal += weights[i]
	}
	if weightTotal == 0 {
		return false
	}

	var signedWeight uint64
	counted := make(map[dids.BlsDID]bool, len(included))
	for _, d := range included {
		if counted[d] {
			continue
		}
		counted[d] = true
		signedWeight += weightByDID[d] // unknown DID → 0
	}

	// GV-L9: overflow-safe 2/3 quorum. The naive `signedWeight*3 >= weightTotal*2`
	// wraps mod 2^64 once weightTotal > MaxUint64/2 (weightTotal*2 wraps to a small
	// value), so a zero-weight commitment could falsely satisfy quorum. The identity
	// ceil(2N/3) == N - floor(N/3) holds for every non-negative N and involves only
	// one subtraction and one division — no intermediate product, so it cannot
	// overflow for any uint64 weight. It is byte-identical to the original on the
	// entire non-overflow domain (weightTotal <= MaxUint64/2), which is the only
	// reachable range at any realistic election scale.
	quorumThreshold := weightTotal - weightTotal/3
	return signedWeight >= quorumThreshold
}
