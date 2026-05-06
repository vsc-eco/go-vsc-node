package settlement

import (
	"reflect"
	"testing"
)

// TestBuildSettlementRecordSortsSubArrays pins the deterministic-ordering
// contract: regardless of caller-side ordering, BuildSettlementRecord emits
// reductions and distributions sorted lexicographically by Account so the
// CBOR/JSON byte representation is identical across nodes.
func TestBuildSettlementRecordSortsSubArrays(t *testing.T) {
	a := BuildSettlementRecord(
		12, 11,
		1000, 1100,
		500, 480, 20,
		[]RewardReductionEntry{{Account: "hive:b", Bps: 25}, {Account: "hive:a", Bps: 50}},
		[]DistributionEntry{{Account: "hive:z", HBDAmt: 1}, {Account: "hive:a", HBDAmt: 2}},
	)
	b := BuildSettlementRecord(
		12, 11,
		1000, 1100,
		500, 480, 20,
		[]RewardReductionEntry{{Account: "hive:a", Bps: 50}, {Account: "hive:b", Bps: 25}},
		[]DistributionEntry{{Account: "hive:a", HBDAmt: 2}, {Account: "hive:z", HBDAmt: 1}},
	)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("expected deterministic equality across input orderings:\n a=%+v\n b=%+v", a, b)
	}
	if a.RewardReductions[0].Account != "hive:a" || a.RewardReductions[1].Account != "hive:b" {
		t.Fatalf("reductions not sorted lexicographically: %+v", a.RewardReductions)
	}
	if a.Distributions[0].Account != "hive:a" || a.Distributions[1].Account != "hive:z" {
		t.Fatalf("distributions not sorted lexicographically: %+v", a.Distributions)
	}
}

// TestBuildSettlementRecordPreservesScalarFields confirms the bookkeeping
// fields are passed through unchanged — these are the on-chain audit values
// for the closing epoch.
func TestBuildSettlementRecordPreservesScalarFields(t *testing.T) {
	rec := BuildSettlementRecord(
		7, 6,
		200, 300,
		1234, 1200, 34,
		nil, nil,
	)
	if rec.Epoch != 7 || rec.PrevEpoch != 6 {
		t.Fatalf("epoch fields wrong: %+v", rec)
	}
	if rec.SnapshotRangeFrom != 200 || rec.SnapshotRangeTo != 300 {
		t.Fatalf("range fields wrong: %+v", rec)
	}
	if rec.BucketBalanceHBD != 1234 || rec.TotalDistributedHBD != 1200 || rec.ResidualHBD != 34 {
		t.Fatalf("amount fields wrong: %+v", rec)
	}
	if rec.TotalDistributedHBD+rec.ResidualHBD != rec.BucketBalanceHBD {
		t.Fatalf("conservation broken: distributed+residual != bucket")
	}
}
