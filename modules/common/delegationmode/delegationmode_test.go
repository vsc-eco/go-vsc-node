package delegationmode

import "testing"

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"deactivated": Deactivated,
		"share":       Share,
		"custom":      Custom,
		// case / whitespace tolerance
		"SHARE":   Share,
		" Custom": Custom,
		"  ":      Deactivated,
		"":        Deactivated,
		// unknown → default (opt-in safe)
		"foo":      Deactivated,
		"disabled": Deactivated,
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDefaultIsDeactivated(t *testing.T) {
	if Default != Deactivated {
		t.Fatalf("Default = %q, want Deactivated (delegation must be strict opt-in)", Default)
	}
}

func TestIsValid(t *testing.T) {
	for _, ok := range []string{Deactivated, Share, Custom} {
		if !IsValid(ok) {
			t.Errorf("IsValid(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "SHARE", " custom", "foo"} {
		if IsValid(bad) {
			t.Errorf("IsValid(%q) = true, want false (IsValid is exact, no normalization)", bad)
		}
	}
}

func TestAllowsDelegation(t *testing.T) {
	// Share and Custom accept third-party delegation; Deactivated (incl.
	// unset/unknown) does not.
	if !AllowsDelegation(Share) {
		t.Error("Share must allow delegation")
	}
	if !AllowsDelegation(Custom) {
		t.Error("Custom must allow delegation")
	}
	if AllowsDelegation(Deactivated) {
		t.Error("Deactivated must reject delegation")
	}
	if AllowsDelegation("") {
		t.Error("empty/default must reject delegation (opt-in)")
	}
	if AllowsDelegation("nonsense") {
		t.Error("unknown mode must reject delegation (opt-in)")
	}
}

func TestSharesRewards(t *testing.T) {
	// Only Share splits pendulum rewards on-chain.
	if !SharesRewards(Share) {
		t.Error("Share must split rewards on-chain")
	}
	if SharesRewards(Custom) {
		t.Error("Custom keeps rewards at the operator (off-chain settlement)")
	}
	if SharesRewards(Deactivated) {
		t.Error("Deactivated must not split rewards")
	}
	if SharesRewards("") {
		t.Error("default must not split rewards")
	}
}

func TestIsAdverseTransition(t *testing.T) {
	// Adverse iff the node LEAVES Share (strips delegators' on-chain reward
	// share). Every other transition is immediate. Full 3x3 truth table over the
	// normalized modes, plus a couple of un-normalized inputs.
	adverse := map[[2]string]bool{
		// old=deactivated: nothing to strip.
		{Deactivated, Deactivated}: false,
		{Deactivated, Share}:       false,
		{Deactivated, Custom}:      false,
		// old=share: leaving Share is adverse; staying / re-announcing is not.
		{Share, Deactivated}: true,
		{Share, Share}:       false,
		{Share, Custom}:      true,
		// old=custom: never shared on-chain, so no reward share to lose —
		// custom->deactivated is an acceptance-only change, NOT adverse.
		{Custom, Deactivated}: false,
		{Custom, Share}:       false,
		{Custom, Custom}:      false,
	}
	for pair, want := range adverse {
		if got := IsAdverseTransition(pair[0], pair[1]); got != want {
			t.Errorf("IsAdverseTransition(%q, %q) = %v, want %v", pair[0], pair[1], got, want)
		}
	}

	// Normalization applies to both operands.
	if !IsAdverseTransition("SHARE", " custom") {
		t.Error("IsAdverseTransition must normalize inputs: SHARE->custom is adverse")
	}
	if IsAdverseTransition("custom", "unknown-mode") {
		t.Error("custom->(unknown=deactivated) is acceptance-only, not adverse")
	}
}
