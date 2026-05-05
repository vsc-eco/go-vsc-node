package settlement

import "testing"

func TestComputeNodeDistributionsResidualToLargestStake(t *testing.T) {
	bonds := map[string]int64{
		"hive:a": 5,
		"hive:b": 3,
		"hive:c": 2,
	}
	out := ComputeNodeDistributions(7, bonds)
	if len(out) != 3 {
		t.Fatalf("expected 3 distributions, got %d", len(out))
	}
	sum := int64(0)
	for _, d := range out {
		sum += d.Amount
	}
	if sum != 7 {
		t.Fatalf("expected sum=7, got %d", sum)
	}
	// 7*(5/10)=3.5 floor 3 + residual 1 => 4
	for _, d := range out {
		if d.Account == "hive:a" && d.Amount != 4 {
			t.Fatalf("expected hive:a residual assignment, got %d", d.Amount)
		}
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
