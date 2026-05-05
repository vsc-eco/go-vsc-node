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
