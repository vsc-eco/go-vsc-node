package settlement

import (
	"testing"
)

// TestComposeRecord_EmptyBucketEmitsMarker covers the no-activity-epoch
// path: bucket == 0, no bonds required, no distributions, no reductions —
// the marker still lands so the next vsc.election_result chain-continuity
// check passes.
func TestComposeRecord_EmptyBucketEmitsMarker(t *testing.T) {
	rec, err := ComposeRecord(ComposeInputs{
		Epoch:        5,
		PrevEpoch:    4,
		EpochStartBh: 1000,
		SlotHeight:   2000,
		// CommitteeBonds intentionally omitted — bucket==0 means we don't
		// need them.
		BucketBalanceHBD:    0,
		ReductionsByAccount: nil,
	})
	if err != nil {
		t.Fatalf("ComposeRecord(empty bucket): unexpected err: %v", err)
	}
	if rec == nil {
		t.Fatal("expected non-nil marker record for empty-activity epoch")
	}
	if rec.Epoch != 5 || rec.PrevEpoch != 4 {
		t.Errorf("epoch chain mismatch: epoch=%d prev=%d", rec.Epoch, rec.PrevEpoch)
	}
	if rec.BucketBalanceHBD != 0 || rec.TotalDistributedHBD != 0 || rec.ResidualHBD != 0 {
		t.Errorf("HBD fields not all zero: bucket=%d total=%d residual=%d",
			rec.BucketBalanceHBD, rec.TotalDistributedHBD, rec.ResidualHBD)
	}
	if len(rec.Distributions) != 0 {
		t.Errorf("expected empty distributions, got %d", len(rec.Distributions))
	}
	if len(rec.RewardReductions) != 0 {
		t.Errorf("expected empty reductions, got %d", len(rec.RewardReductions))
	}
	if rec.SnapshotRangeFrom != 1000 || rec.SnapshotRangeTo != 2000 {
		t.Errorf("snapshot range mismatch: from=%d to=%d", rec.SnapshotRangeFrom, rec.SnapshotRangeTo)
	}
}

// TestComposeRecord_EmptyBucketIgnoresEmptyBonds confirms the bonds-required
// gate is skipped on the empty-bucket path. A chain in a degraded state
// (election with members but transient bond-read failure) would otherwise
// permanently stall epoch progression on a no-activity epoch.
func TestComposeRecord_EmptyBucketIgnoresEmptyBonds(t *testing.T) {
	rec, err := ComposeRecord(ComposeInputs{
		Epoch:               5,
		PrevEpoch:           4,
		EpochStartBh:        1000,
		SlotHeight:           2000,
		CommitteeBonds:      map[string]int64{}, // explicitly empty
		BucketBalanceHBD:    0,
		ReductionsByAccount: nil,
	})
	if err != nil || rec == nil {
		t.Fatalf("expected marker record despite empty bonds; err=%v rec=%v", err, rec)
	}
}

// TestComposeRecord_NonEmptyBucketStillRequiresBonds confirms we only relax
// the bonds requirement for empty-activity epochs — a non-empty bucket
// without bonds still errors out.
func TestComposeRecord_NonEmptyBucketStillRequiresBonds(t *testing.T) {
	_, err := ComposeRecord(ComposeInputs{
		Epoch:               5,
		PrevEpoch:           4,
		EpochStartBh:        1000,
		SlotHeight:           2000,
		CommitteeBonds:      map[string]int64{},
		BucketBalanceHBD:    1000,
		ReductionsByAccount: nil,
	})
	if err == nil {
		t.Fatal("expected error for non-empty bucket with empty bonds")
	}
}

// TestComposeRecord_RejectsNegativeBucket guards against a corrupted
// upstream read producing a negative bucket — refuse rather than emit a
// nonsense record.
func TestComposeRecord_RejectsNegativeBucket(t *testing.T) {
	_, err := ComposeRecord(ComposeInputs{
		Epoch:               5,
		PrevEpoch:           4,
		EpochStartBh:        1000,
		SlotHeight:           2000,
		BucketBalanceHBD:    -1,
		ReductionsByAccount: nil,
	})
	if err == nil {
		t.Fatal("expected error for negative bucket")
	}
}

// TestComposeRecord_EmptyBucket_DeterministicAcrossCalls confirms two
// invocations with the same inputs produce byte-equal records — required
// for the apply-time re-derivation byte-compare.
func TestComposeRecord_EmptyBucket_DeterministicAcrossCalls(t *testing.T) {
	in := ComposeInputs{
		Epoch:               7,
		PrevEpoch:           6,
		EpochStartBh:        1000,
		SlotHeight:           1500,
		BucketBalanceHBD:    0,
		ReductionsByAccount: nil,
	}
	r1, err := ComposeRecord(in)
	if err != nil {
		t.Fatalf("first compose: %v", err)
	}
	r2, err := ComposeRecord(in)
	if err != nil {
		t.Fatalf("second compose: %v", err)
	}
	if r1.Epoch != r2.Epoch ||
		r1.PrevEpoch != r2.PrevEpoch ||
		r1.SnapshotRangeFrom != r2.SnapshotRangeFrom ||
		r1.SnapshotRangeTo != r2.SnapshotRangeTo ||
		r1.BucketBalanceHBD != r2.BucketBalanceHBD ||
		r1.TotalDistributedHBD != r2.TotalDistributedHBD ||
		r1.ResidualHBD != r2.ResidualHBD ||
		len(r1.Distributions) != len(r2.Distributions) ||
		len(r1.RewardReductions) != len(r2.RewardReductions) {
		t.Errorf("compose not deterministic: r1=%+v r2=%+v", r1, r2)
	}
}
