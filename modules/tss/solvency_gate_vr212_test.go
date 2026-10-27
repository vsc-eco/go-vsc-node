package tss

import "testing"

// VR2-12: the pure pre-activation-reshare decision. From 0.12.0 a pending gen in the
// skip set may reshare once its check-sig is verified; superseded gens stay skipped.
// Below 0.12.0 a pending gen stays skipped (VR2-10), whatever its check-sig state.
func TestSkipReshareDecision_VR212(t *testing.T) {
	cases := []struct {
		name                                          string
		inSkip, isPending, sigVerified, preActivation bool
		want                                          bool
	}{
		{"not in skip set -> reshares (active gen)", false, false, false, true, false},
		{"not in skip set, pending+verified -> reshares", false, true, true, true, false},
		{"superseded (in skip, not pending) -> skip forever", true, false, false, true, true},
		{"superseded with a stray verified flag -> still skip (not pending)", true, false, true, true, true},
		{"pending, check-sig NOT verified -> skip until verified", true, true, false, true, true},
		{"pending, check-sig verified -> reshare before activation", true, true, true, true, false},
		{"below 0.12: pending+verified stays skipped (VR2-10)", true, true, true, false, true},
		{"below 0.12: superseded stays skipped", true, false, false, false, true},
		{"below 0.12: not in skip set -> reshares", false, false, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := skipReshareDecision(tc.inSkip, tc.isPending, tc.sigVerified, tc.preActivation); got != tc.want {
				t.Fatalf("skipReshareDecision(%v,%v,%v,%v) = %v, want %v",
					tc.inSkip, tc.isPending, tc.sigVerified, tc.preActivation, got, tc.want)
			}
		})
	}
}
