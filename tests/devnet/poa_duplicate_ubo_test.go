package devnet

import (
	"context"
	"strings"
	"testing"
	"time"

	"vsc-node/modules/common/params"
)

// TestPoaDuplicateUboRefused: one beneficial owner, one seat.
//
// A seat admitted with ubo_id X must block any later admission that reuses X
// (poa_admission.go: "a UBO that already holds a seat: the vote is moot"), and the
// UBO is case-folded and trimmed before the comparison, so " UBO-X " must collide too.
// Positive control first: the first candidate with X must actually be admitted,
// otherwise the refusals below prove nothing.
func TestPoaDuplicateUboRefused(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}
	cfg := tssTestConfig()
	if cfg.SysConfigOverrides.ConsensusParams == nil {
		cfg.SysConfigOverrides.ConsensusParams = &params.ConsensusParams{}
	}
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorMajor = 0
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorConsensus = 7
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorEpoch = 1
	d, ctx := startDevnetNoKey(t, cfg, 45*time.Minute)

	for n := 1; n <= cfg.Nodes; n++ {
		nctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		err := d.waitForElectionEpoch(nctx, n, 2, 10*time.Minute)
		cancel()
		if err != nil {
			t.Fatalf("magi-%d never reached epoch 2: %v", n, err)
		}
	}
	seats0 := mustSeats(t, d, ctx, 1)
	total := uint64(len(seats0))
	required := int(total - total/3)
	t.Logf("registry %d seats, admission bar %d", total, required)

	seated := func(acct string) (bool, string) {
		for _, s := range mustSeats(t, d, ctx, 1) {
			if s.Account == acct {
				return true, s.UboId
			}
		}
		return false, ""
	}
	vote := func(cand, ubo string) {
		for v := 1; v <= required; v++ {
			if _, err := d.admitVote(v, cand, ubo); err != nil {
				t.Fatalf("admit_vote magi-%d for %s: %v", v, cand, err)
			}
		}
		time.Sleep(60 * time.Second)
	}

	// positive control: first holder of the UBO is admitted
	vote("magi.v7a", "ubo-v7")
	ok, ubo := seated("magi.v7a")
	if !ok {
		t.Fatalf("PRECONDITION FAILED: magi.v7a was not admitted on %d votes; the refusals below would be vacuous", required)
	}
	t.Logf("magi.v7a admitted with ubo_id %q", ubo)

	// V7: same UBO, different candidate
	vote("magi.v7b", "ubo-v7")
	if ok, _ := seated("magi.v7b"); ok {
		t.Errorf("V7 FAILED: magi.v7b was admitted reusing ubo-v7 (one owner now holds two seats)")
	} else {
		t.Logf("V7 PASS: magi.v7b refused (same ubo_id)")
	}

	// normalization: case + surrounding spaces must collide with the stored UBO
	vote("magi.v7c", "  "+strings.ToUpper("ubo-v7")+" ")
	if ok, _ := seated("magi.v7c"); ok {
		t.Errorf("V7 NORMALIZATION FAILED: magi.v7c admitted with %q, which should fold to ubo-v7", " UBO-V7 ")
	} else {
		t.Logf("V7 normalization PASS: magi.v7c refused (UBO folds to ubo-v7)")
	}
}
