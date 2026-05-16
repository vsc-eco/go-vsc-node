package oracle

import (
	"testing"
)

type stubPools struct {
	reserves map[string]int64
}

func (s *stubPools) ReadPoolHBDReserve(contractID string, blockHeight uint64) (int64, bool) {
	v, ok := s.reserves[contractID]
	if !ok {
		return 0, false
	}
	return v, true
}

type stubBonds struct {
	members []string
	total   int64
}

func (s *stubBonds) ReadCommitteeBond(blockHeight uint64) ([]string, int64) {
	return append([]string(nil), s.members...), s.total
}

func happyInputs() GeometryInputs {
	return GeometryInputs{
		BlockHeight:       1_000,
		HivePriceHBDBps:   3_000, // 0.30 HBD per HIVE
		HivePriceOK:       true,
		WhitelistedPools:  []string{"vsc1Pool1", "vsc1Pool2"},
		EffectiveStakeNum: 2,
		EffectiveStakeDen: 3,
	}
}

func TestGeometry_HappyPath(t *testing.T) {
	pools := &stubPools{reserves: map[string]int64{
		"vsc1Pool1": 1_000_000,
		"vsc1Pool2": 500_000,
	}}
	bonds := &stubBonds{members: []string{"hive:a", "hive:b"}, total: 10_000_000}
	g := NewGeometryComputer(pools, bonds)

	out := g.Compute(happyInputs())
	if !out.OK {
		t.Fatalf("expected OK geometry, got %+v", out)
	}

	wantP := int64(1_500_000)
	if out.P != wantP {
		t.Errorf("P: got %d want %d", out.P, wantP)
	}
	if out.V != 2*wantP {
		t.Errorf("V: got %d want %d", out.V, 2*wantP)
	}
	if out.T != 10_000_000 {
		t.Errorf("T: got %d want 10000000", out.T)
	}
	// E = T * price * 2/3 = 10_000_000 * 0.30 * 2/3 = 2_000_000
	if out.E != 2_000_000 {
		t.Errorf("E: got %d want 2000000", out.E)
	}
	// s = V/E = 3_000_000 / 2_000_000 = 1.5 → 15_000 bps
	if out.SBps != 15_000 {
		t.Errorf("SBps: got %d want 15000", out.SBps)
	}
}

func TestGeometry_PoolWithoutPublishedReserveSkipped(t *testing.T) {
	// Only one of two pools has published a reserve — the missing one is
	// silently skipped so a freshly-deployed pool doesn't poison the geometry.
	pools := &stubPools{reserves: map[string]int64{
		"vsc1Pool1": 1_000_000,
		// vsc1Pool2 missing
	}}
	bonds := &stubBonds{total: 10_000_000}
	g := NewGeometryComputer(pools, bonds)

	out := g.Compute(happyInputs())
	if !out.OK || out.P != 1_000_000 {
		t.Fatalf("expected P=1000000 from one pool, got %+v", out)
	}
}

func TestGeometry_NoPoolsYieldsZeroVCliffPath(t *testing.T) {
	pools := &stubPools{reserves: map[string]int64{}}
	bonds := &stubBonds{total: 10_000_000}
	g := NewGeometryComputer(pools, bonds)

	out := g.Compute(happyInputs())
	if !out.OK {
		t.Fatalf("expected OK with V=0, got %+v", out)
	}
	if out.V != 0 || out.P != 0 {
		t.Errorf("expected V=P=0, got V=%d P=%d", out.V, out.P)
	}
	if out.SBps != 0 {
		t.Errorf("expected SBps=0 when V=0, got %d", out.SBps)
	}
}

func TestGeometry_HivePriceMissingFails(t *testing.T) {
	pools := &stubPools{reserves: map[string]int64{"vsc1Pool1": 1_000_000}}
	bonds := &stubBonds{total: 10_000_000}
	g := NewGeometryComputer(pools, bonds)

	in := happyInputs()
	in.HivePriceOK = false
	out := g.Compute(in)
	if out.OK {
		t.Fatalf("expected !OK when hive price missing, got %+v", out)
	}
}

func TestGeometry_NoBondedCommitteeFails(t *testing.T) {
	pools := &stubPools{reserves: map[string]int64{"vsc1Pool1": 1_000_000}}
	bonds := &stubBonds{total: 0}
	g := NewGeometryComputer(pools, bonds)

	out := g.Compute(happyInputs())
	if out.OK {
		t.Fatalf("expected !OK when committee has zero bond, got %+v", out)
	}
}

func TestGeometry_DegenerateEffectiveFractionFallsBackToTwoThirds(t *testing.T) {
	pools := &stubPools{reserves: map[string]int64{"vsc1Pool1": 1_500_000}}
	bonds := &stubBonds{total: 10_000_000}
	g := NewGeometryComputer(pools, bonds)

	in := happyInputs()
	in.EffectiveStakeNum = 0
	in.EffectiveStakeDen = 0
	out := g.Compute(in)
	if !out.OK || out.E != 2_000_000 {
		t.Fatalf("expected fallback to 2/3 → E=2000000, got %+v", out)
	}
}

func TestGeometry_DeterministicAcrossPoolOrder(t *testing.T) {
	pools := &stubPools{reserves: map[string]int64{
		"vsc1Z": 100_000,
		"vsc1A": 200_000,
		"vsc1M": 300_000,
	}}
	bonds := &stubBonds{total: 10_000_000}
	g := NewGeometryComputer(pools, bonds)

	in1 := happyInputs()
	in1.WhitelistedPools = []string{"vsc1Z", "vsc1A", "vsc1M"}
	in2 := happyInputs()
	in2.WhitelistedPools = []string{"vsc1A", "vsc1M", "vsc1Z"}

	out1 := g.Compute(in1)
	out2 := g.Compute(in2)
	if out1 != out2 {
		t.Fatalf("non-deterministic geometry across pool order: %+v vs %+v", out1, out2)
	}
}

func TestGeometry_EmptyWhitelistAllowedWhenOtherInputsValid(t *testing.T) {
	pools := &stubPools{}
	bonds := &stubBonds{total: 10_000_000}
	g := NewGeometryComputer(pools, bonds)

	in := happyInputs()
	in.WhitelistedPools = nil
	out := g.Compute(in)
	if !out.OK || out.V != 0 || out.E != 2_000_000 {
		t.Fatalf("expected OK with V=0, E=2_000_000; got %+v", out)
	}
}

func TestGeometry_NilComputerSafe(t *testing.T) {
	var g *GeometryComputer
	out := g.Compute(happyInputs())
	if out.OK {
		t.Fatalf("nil computer should return zero geometry, got %+v", out)
	}
}
