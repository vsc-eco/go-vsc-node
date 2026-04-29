package pendulum_oracle

import (
	"bytes"
	"testing"

	"go.mongodb.org/mongo-driver/bson"
)

// TestSnapshotRecord_BsonStableForIdenticalInputs confirms two records built
// from identical fields marshal to identical bson bytes. This is the W2
// consensus-boundary guarantee: every node persists the same bytes for the
// same tick.
func TestSnapshotRecord_BsonStableForIdenticalInputs(t *testing.T) {
	rec := SnapshotRecord{
		TickBlockHeight:     12345,
		TrustedHiveMean:     30_000_000,
		TrustedHiveOK:       true,
		HiveMovingAvg:       29_500_000,
		HiveMovingAvgOK:     true,
		HBDInterestRateBps:  1500,
		HBDInterestRateOK:   true,
		TrustedWitnessGroup: []string{"alice", "bob", "carol"},
		WitnessSlashBps: []WitnessSlashRecord{
			{Witness: "alice", Bps: 0},
			{Witness: "bob", Bps: 25},
			{Witness: "carol", Bps: 50},
		},
	}

	a, err := bson.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal a: %v", err)
	}
	b, err := bson.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal b: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("marshaling not stable: %x vs %x", a, b)
	}
}

func TestSnapshotRecord_RoundTripsBson(t *testing.T) {
	rec := SnapshotRecord{
		TickBlockHeight:     999,
		TrustedHiveMean:     -1234,
		TrustedHiveOK:       true,
		HiveMovingAvg:       0,
		HiveMovingAvgOK:     false,
		HBDInterestRateBps:  2000,
		HBDInterestRateOK:   true,
		TrustedWitnessGroup: []string{"x"},
		WitnessSlashBps:     []WitnessSlashRecord{{Witness: "x", Bps: 17}},
	}
	raw, err := bson.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got SnapshotRecord
	if err := bson.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.TickBlockHeight != rec.TickBlockHeight ||
		got.TrustedHiveMean != rec.TrustedHiveMean ||
		got.TrustedHiveOK != rec.TrustedHiveOK ||
		got.HBDInterestRateBps != rec.HBDInterestRateBps ||
		len(got.WitnessSlashBps) != 1 ||
		got.WitnessSlashBps[0].Witness != "x" ||
		got.WitnessSlashBps[0].Bps != 17 {
		t.Fatalf("round trip lost fields: got %+v want %+v", got, rec)
	}
}
