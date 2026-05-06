package safetyslash

import "testing"

func TestEffectiveCorrelatedBps_Caps(t *testing.T) {
	if g := EffectiveCorrelatedBps([]int{3000, 4000, 5000}, 10000); g != 10000 {
		t.Fatalf("expected cap 10000, got %d", g)
	}
	if g := EffectiveCorrelatedBps([]int{100, 200}, 500); g != 300 {
		t.Fatalf("expected sum 300, got %d", g)
	}
	if g := EffectiveCorrelatedBps([]int{-5, 100}, 10000); g != 100 {
		t.Fatalf("expected negatives ignored, got %d", g)
	}
}

func TestSlashPolicy_KindMappings(t *testing.T) {
	cases := []struct {
		kind      string
		wantBps   int
		threshold bool
		wantCount int
	}{
		{EvidenceSettlementPayloadFraud, SettlementFraudSlashBps, false, 1},
		{EvidenceTSSEquivocation, TSSEquivocationSlashBps, true, TSSEquivocationThresholdCount},
		{EvidenceVSCDoubleBlockSign, DoubleBlockSignSlashBps, false, 1},
		{EvidenceVSCInvalidBlockProposal, InvalidBlockSlashBps, false, 1},
		{EvidenceOraclePayloadFraud, OraclePayloadSlashBps, true, OraclePayloadThresholdCount},
	}
	for _, tc := range cases {
		if got := SlashBpsForEvidenceKind(tc.kind); got != tc.wantBps {
			t.Fatalf("kind %s: bps got %d want %d", tc.kind, got, tc.wantBps)
		}
		if got := UsesThreshold(tc.kind); got != tc.threshold {
			t.Fatalf("kind %s: threshold got %v want %v", tc.kind, got, tc.threshold)
		}
		if got := ThresholdCountForEvidenceKind(tc.kind); got != tc.wantCount {
			t.Fatalf("kind %s: threshold count got %d want %d", tc.kind, got, tc.wantCount)
		}
	}
}
