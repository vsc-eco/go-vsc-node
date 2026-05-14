package settlement

import "testing"

func TestComputeNodeDistributionsFloorsAndLeavesRemainder(t *testing.T) {
	bonds := map[string]int64{
		"hive:a": 5,
		"hive:b": 3,
		"hive:c": 2,
	}
	// total=10, nodeShare=7 → floors: a=3 (7*5/10), b=2 (7*3/10), c=1 (7*2/10).
	// sum=6; the leftover base unit is NOT assigned to anyone — ComposeRecord
	// captures it as ResidualHBD and it rolls into the next epoch.
	out := ComputeNodeDistributions(7, bonds)
	if len(out) != 3 {
		t.Fatalf("expected 3 distributions, got %d", len(out))
	}
	want := map[string]int64{"hive:a": 3, "hive:b": 2, "hive:c": 1}
	sum := int64(0)
	for _, d := range out {
		if d.Amount != want[d.Account] {
			t.Fatalf("%s: expected %d, got %d", d.Account, want[d.Account], d.Amount)
		}
		sum += d.Amount
	}
	if sum != 6 {
		t.Fatalf("expected sum=6 (remainder of 1 left unassigned), got %d", sum)
	}
}

// TestComputeNodeDistributionsEqualStakeNoTieBreakAdvantage pins the property
// the rollover change was made for: an equal-stake committee splits exactly
// equally, with no base-unit advantage to whoever sorts first.
func TestComputeNodeDistributionsEqualStakeNoTieBreakAdvantage(t *testing.T) {
	bonds := map[string]int64{
		"hive:magi.test1": 100,
		"hive:magi.test2": 100,
		"hive:magi.test3": 100,
		"hive:magi.test4": 100,
		"hive:magi.test5": 100,
	}
	// nodeShare=6, 5 equal nodes → each floor(6*100/500)=1, sum=5, remainder=1.
	out := ComputeNodeDistributions(6, bonds)
	sum := int64(0)
	for _, d := range out {
		if d.Amount != 1 {
			t.Fatalf("%s: expected equal share 1, got %d", d.Account, d.Amount)
		}
		sum += d.Amount
	}
	if sum != 5 {
		t.Fatalf("expected sum=5 (remainder of 1 rolls over), got %d", sum)
	}
}

func TestApplyRewardReductionsToBonds(t *testing.T) {
	bonds := map[string]int64{
		"hive:a": 1000,
		"hive:b": 500,
	}
	post, applied := ApplyRewardReductionsToBonds(bonds, map[string]int{
		"hive:a": 250, // 2.5%
		"hive:b": 0,
	})
	if post["hive:a"] != 975 {
		t.Fatalf("expected hive:a effective bond 975, got %d", post["hive:a"])
	}
	if post["hive:b"] != 500 {
		t.Fatalf("expected hive:b unchanged, got %d", post["hive:b"])
	}
	if len(applied) != 1 {
		t.Fatalf("expected one reduction record, got %d", len(applied))
	}
}

func TestApplyRewardReductionsToBonds_FullCap(t *testing.T) {
	// 100% reduction (10000 bps) zeroes the effective bond.
	bonds := map[string]int64{"hive:a": 1000}
	post, applied := ApplyRewardReductionsToBonds(bonds, map[string]int{"hive:a": 10000})
	if post["hive:a"] != 0 {
		t.Fatalf("expected effective bond 0 at 10000 bps, got %d", post["hive:a"])
	}
	if applied[0].ReductionAmount != 1000 {
		t.Fatalf("expected 1000 reduction, got %d", applied[0].ReductionAmount)
	}
}

func TestCalculateSplitPreviewFixedConservesR(t *testing.T) {
	out := CalculateSplitPreviewFixed(1000, 900, 2, 3, 0, 0)
	if out.E != 600 {
		t.Fatalf("expected E=600, got %d", out.E)
	}
	if out.FinalNodeShare+out.FinalPoolShare != out.R {
		t.Fatalf("split does not conserve R: node=%d pool=%d R=%d", out.FinalNodeShare, out.FinalPoolShare, out.R)
	}
}
