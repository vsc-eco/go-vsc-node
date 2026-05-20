package election_proposer

// computeRequiredMemberWeight returns the per-required-member voting weight
// the election proposer assigns to accounts in REQUIRED_ELECTION_MEMBERS.
//
// review4 HIGH #6: the prior form used float64 arithmetic
//
//	math.Ceil((1 + float64(opt)/2) / float64(n))
//
// directly inside the election weight that feeds the BLS circuit. Float
// rounding could differ at the boundary across CPU architectures (x86
// FMA / SSE vs ARM / different libm impls), producing divergent election
// bytes and CIDs across otherwise-honest nodes. The integer form below
// computes the identical value with no float ops:
//
//	ceil((1 + opt/2) / n) == ceil((opt + 2) / (2n))
//	                     == ((opt + 2) + (2n - 1)) / (2n)
//	                     == (opt + 2n + 1) / (2n)
//
// for non-negative opt and n >= 1.
func computeRequiredMemberWeight(totalOptionalWeight uint64, numRequired uint64) uint64 {
	if numRequired == 0 {
		return 0
	}
	denom := 2 * numRequired
	return (totalOptionalWeight + denom + 1) / denom
}
