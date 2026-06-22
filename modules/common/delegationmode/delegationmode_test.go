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
