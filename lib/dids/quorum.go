package dids

// This file holds the one weight fold every consensus site shares when it asks
// "did enough of the elected weight actually sign this?".
//
// Why it has to be shared. A BLS aggregate over a committee containing the SAME
// key at two seats verifies by construction, and no attacker cooperation is
// needed to build one. BlsCircuit.aggregateSignaturesFrom walks the keyset by
// INDEX while signatures are keyed by DID, so one honest signature matches a
// duplicated DID at both indices: it lands in the slice twice, both bits are set,
// and bls.Aggregate is a plain EC point sum with no duplicate check. The result
// is 2S against a keyset summing to 2P, and by bilinearity
// e(2P, H(m)) == e(G, 2S) — so verification passes. The honest validator never
// signs twice and never knows.
//
// The signature check is therefore NOT where a duplicate is caught. It has to be
// caught in the weight fold, and every site that folds weight has to do it the
// same way — six sites each inventing their own tie-break is how the previous
// defect happened: one summed both slots, one kept the last write (which the
// attacker chooses, since members are account-sorted), and a third kept the
// first.
//
// Deliberately NOT fail-closed on a collision. Refusing to produce a verdict
// would convert a weight-inflation bug into a chain-halt: BlsQuorumMet gates TSS
// commitments, and the block fold drives BlockInvalid, which SLASHES the
// proposer. Anyone able to seat one duplicate key could then stall the network
// and get honest proposers slashed. Dropping the duplicate seat is both safer
// and more accurate — a duplicated key is ONE signing entity, so it gets ONE
// seat's weight, and the phantom seat contributes nothing to either side of the
// ratio.

// FoldSignedWeight computes how much elected weight signed, and the total weight
// that could have, with duplicate member keys collapsed to a single seat.
//
// memberKeys and weights are the on-chain election in index order; included is
// the set of DIDs the verified BLS aggregate says signed. ok is false when the
// input cannot yield a meaningful verdict — a length mismatch, an empty
// committee, or zero total weight — and callers must treat that as "no quorum".
//
// On a duplicate key the FIRST occurrence wins, in the numerator and the
// denominator alike. First, not last, because members arrive sorted by account,
// so first-wins is the lexicographically-first account — the same tie-break the
// admission-side dedup documents. Last-wins would hand the choice to an attacker,
// who need only pick an account name sorting after their victim's.
//
// An unknown DID in included contributes zero, and a repeated DID is counted
// once, so an inflated bitset cannot manufacture weight.
func FoldSignedWeight(memberKeys []string, weights []uint64, included []BlsDID) (signed, total uint64, ok bool) {
	if len(memberKeys) == 0 || len(memberKeys) != len(weights) {
		return 0, 0, false
	}

	weightByDID := make(map[BlsDID]uint64, len(memberKeys))
	for i, key := range memberKeys {
		did := BlsDID(key)
		if _, seen := weightByDID[did]; seen {
			// A duplicate seat. It cannot sign independently of the seat that
			// already holds this key, so it is not a distinct signer and carries
			// no weight. Skipping it here removes it from `total` as well, which
			// is the point: leaving it in the denominator would let an attacker
			// raise the bar honest signers must clear.
			continue
		}
		weightByDID[did] = weights[i]
		total += weights[i]
	}
	if total == 0 {
		return 0, 0, false
	}

	counted := make(map[BlsDID]bool, len(included))
	for _, did := range included {
		if counted[did] {
			continue
		}
		counted[did] = true
		signed += weightByDID[did] // unknown DID -> 0
	}
	return signed, total, true
}

// TwoThirdsMet reports whether signed weight reaches the 2/3 supermajority of
// total.
//
// GV-L9: computed as total - total/3 rather than the naive signed*3 >= total*2.
// The product form wraps mod 2^64 once total exceeds MaxUint64/2, at which point
// a ZERO-weight commitment can satisfy the comparison. The identity
// ceil(2N/3) == N - floor(N/3) holds for every non-negative N, uses one
// subtraction and one division, and so cannot overflow for any uint64 — while
// staying byte-identical to the product form across the entire non-overflow
// domain, which is the only range any realistic election reaches.
func TwoThirdsMet(signed, total uint64) bool {
	if total == 0 {
		return false
	}
	return signed >= total-total/3
}

// QuorumMet folds the weight and applies the 2/3 rule in one call — the form
// most callers want.
func QuorumMet(memberKeys []string, weights []uint64, included []BlsDID) bool {
	signed, total, ok := FoldSignedWeight(memberKeys, weights, included)
	if !ok {
		return false
	}
	return TwoThirdsMet(signed, total)
}
