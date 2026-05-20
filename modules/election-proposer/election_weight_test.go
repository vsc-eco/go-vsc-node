package election_proposer

import (
	"math"
	"testing"
)

// floatFormula reproduces the pre-fix float form, kept here as the test
// oracle. The integer reformulation must match its result exactly for every
// (opt, n) pair we expect at runtime.
func floatFormula(opt, n uint64) uint64 {
	if n == 0 {
		return 0
	}
	return uint64(math.Ceil((1 + float64(opt)/2) / float64(n)))
}

func TestComputeRequiredMemberWeight_MatchesFloatOracle(t *testing.T) {
	// Cover boundary cases (zero, one) plus the mainnet operating range:
	// totalOptionalWeight scales with the number of optional witnesses
	// (~17 active * per-witness weight up to a few hundred), and
	// REQUIRED_ELECTION_MEMBERS is small (single digits).
	optCases := []uint64{0, 1, 2, 3, 5, 7, 10, 50, 99, 100, 101, 200, 333, 1000, 9999, 100000}
	nCases := []uint64{0, 1, 2, 3, 5, 7}
	for _, opt := range optCases {
		for _, n := range nCases {
			got := computeRequiredMemberWeight(opt, n)
			want := floatFormula(opt, n)
			if got != want {
				t.Fatalf("opt=%d n=%d: got %d, want %d (float oracle)", opt, n, got, want)
			}
		}
	}
}

func TestComputeRequiredMemberWeight_LargeValuesNoOverflow(t *testing.T) {
	// Sanity check that the integer reformulation doesn't blow up on
	// values larger than the float64 mantissa (2^53). We're well below
	// uint64 wrap territory here; the assertion is just that the function
	// returns a sensible value.
	const bigOpt uint64 = 1 << 60
	const n uint64 = 3
	got := computeRequiredMemberWeight(bigOpt, n)
	// ceil((bigOpt + 2) / 6)
	want := (bigOpt + 2 + 5) / 6
	if got != want {
		t.Fatalf("bigOpt=%d n=%d: got %d, want %d", bigOpt, n, got, want)
	}
}

func TestComputeRequiredMemberWeight_ZeroRequiredReturnsZero(t *testing.T) {
	if got := computeRequiredMemberWeight(100, 0); got != 0 {
		t.Fatalf("n=0 must return 0, got %d", got)
	}
}
