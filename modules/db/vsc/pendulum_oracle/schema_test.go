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
		TrustedHivePriceBps: 3_000,
		TrustedHiveOK:       true,
		HiveMovingAvgBps:    2_950,
		HiveMovingAvgOK:     true,
		HBDInterestRateBps:  1500,
		HBDInterestRateOK:   true,
		TrustedWitnessGroup: []string{"alice", "bob", "carol"},
		WitnessRewardReductions: []WitnessRewardReductionRecord{
			{Witness: "alice", Bps: 0},
			{Witness: "bob", Bps: 25, Evidence: WitnessLivenessEvidence{BlockAttestationBps: 25}},
			{Witness: "carol", Bps: 200, Evidence: WitnessLivenessEvidence{BlockProductionBps: 200}},
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
		TrustedHivePriceBps: -1234,
		TrustedHiveOK:       true,
		HiveMovingAvgBps:    0,
		HiveMovingAvgOK:     false,
		HBDInterestRateBps:  2000,
		HBDInterestRateOK:   true,
		TrustedWitnessGroup: []string{"x"},
		WitnessRewardReductions: []WitnessRewardReductionRecord{
			{Witness: "x", Bps: 17, Evidence: WitnessLivenessEvidence{TssBlameBps: 17}},
		},
		GeometryOK: true,
		GeometryV:  500_000,
		GeometryP:  250_000,
		GeometryE:  750_000,
		GeometryT:    1_000_000,
		GeometrySBps: 6_667,
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
		got.TrustedHivePriceBps != rec.TrustedHivePriceBps ||
		got.TrustedHiveOK != rec.TrustedHiveOK ||
		got.HBDInterestRateBps != rec.HBDInterestRateBps ||
		len(got.WitnessRewardReductions) != 1 ||
		got.WitnessRewardReductions[0].Witness != "x" ||
		got.WitnessRewardReductions[0].Bps != 17 ||
		got.WitnessRewardReductions[0].Evidence.TssBlameBps != 17 ||
		got.GeometryOK != rec.GeometryOK ||
		got.GeometryV != rec.GeometryV ||
		got.GeometryP != rec.GeometryP ||
		got.GeometryE != rec.GeometryE ||
		got.GeometryT != rec.GeometryT ||
		got.GeometrySBps != rec.GeometrySBps {
		t.Fatalf("round trip lost fields: got %+v want %+v", got, rec)
	}
}
