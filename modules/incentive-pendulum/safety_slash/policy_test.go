package safetyslash

import (
	"testing"

	"vsc-node/modules/common/params"
)

// TestSlashPolicy_LeanConstantsMirroredInGo pins the literal values that the
// Magi Lean modules `SafetySlashLiquidSplit.lean` (`maxSafetySlashBurnDelayBlocks`)
// and policy doc-comments hard-code. If either side changes, this test fires and
// the maintainer must update both manually — there is no automatic build-time
// link between the two repos.
func TestSlashPolicy_LeanConstantsMirroredInGo(t *testing.T) {
	if params.MaxSafetySlashBurnDelayBlocks != 3_333_333 {
		t.Fatalf("params.MaxSafetySlashBurnDelayBlocks drifted from Lean's maxSafetySlashBurnDelayBlocks: got %d, want 3_333_333. "+
			"Update magi-lean/MagiLean/Pendulum/SafetySlashLiquidSplit.lean in lockstep.",
			params.MaxSafetySlashBurnDelayBlocks)
	}
	if DefaultSafetySlashBurnDelayBlocks != 3*28800 {
		t.Fatalf("DefaultSafetySlashBurnDelayBlocks drifted: got %d, want %d. "+
			"Lean docs reference ~3 days at 3s/block; update both sides.",
			DefaultSafetySlashBurnDelayBlocks, 3*28800)
	}
	if CorrelatedSlashCapBps != 10000 {
		t.Fatalf("CorrelatedSlashCapBps drifted from 100%% (10000 bps): got %d", CorrelatedSlashCapBps)
	}
	if DoubleBlockSignSlashBps != 1000 || InvalidBlockSlashBps != 1000 {
		t.Fatalf("Per-kind slash bps drifted from Lean slashBps_uniform_10pct: doubleBlock=%d invalidBlock=%d",
			DoubleBlockSignSlashBps, InvalidBlockSlashBps)
	}
}

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
		kind    string
		wantBps int
	}{
		{EvidenceVSCDoubleBlockSign, DoubleBlockSignSlashBps},
		{EvidenceVSCInvalidBlockProposal, InvalidBlockSlashBps},
	}
	for _, tc := range cases {
		if got := SlashBpsForEvidenceKind(tc.kind); got != tc.wantBps {
			t.Fatalf("kind %s: bps got %d want %d", tc.kind, got, tc.wantBps)
		}
	}
}

// TestSlashPolicy_UnknownKindZeroBps documents that retired/unknown evidence
// strings (e.g. the reserved "settlement_payload_fraud", "tss_equivocation",
// "oracle_payload_fraud") yield zero bps, which slashForEvidenceIfPolicyAllows
// rejects — keeping retired kinds inert if they ever leak from old metadata.
func TestSlashPolicy_UnknownKindZeroBps(t *testing.T) {
	for _, k := range []string{
		"settlement_payload_fraud",
		"tss_equivocation",
		"oracle_payload_fraud",
		"",
		"unknown_kind",
	} {
		if got := SlashBpsForEvidenceKind(k); got != 0 {
			t.Fatalf("unknown kind %q: expected 0 bps, got %d", k, got)
		}
	}
}
