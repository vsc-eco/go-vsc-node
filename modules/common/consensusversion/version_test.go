package consensusversion

import "testing"

func TestCmp(t *testing.T) {
	a := Version{1, 2, 3}
	b := Version{1, 2, 4}
	if a.Cmp(b) >= 0 {
		t.Fatal()
	}
	if b.Cmp(a) <= 0 {
		t.Fatal()
	}
	if a.Cmp(a) != 0 {
		t.Fatal()
	}
}

func TestAtLeast(t *testing.T) {
	v := Version{1, 5, 0}
	if !v.AtLeast(Version{1, 4, 0}) {
		t.Fatal()
	}
	if v.AtLeast(Version{1, 6, 0}) {
		t.Fatal()
	}
}

func TestFormatProvisional(t *testing.T) {
	s := FormatProvisional(Version{1, 3, 7})
	if s != "1.4.0-p" {
		t.Fatalf("got %q", s)
	}
}

func TestMaxComponentwise(t *testing.T) {
	m := MaxComponentwise(Version{2, 1, 0}, Version{1, 5, 3})
	if m != (Version{2, 5, 3}) {
		t.Fatalf("got %+v", m)
	}
}

// RunningVersion is compiled in from source constants (not ldflags), so it must equal the
// pinned current triple. Update this pin in the SAME commit that bumps the constants in
// version.go.
func TestRunningVersionIsSourcePinned(t *testing.T) {
	want := Version{Major: 0, Consensus: 2, NonConsensus: 0}
	if got := RunningVersion(); got != want {
		t.Fatalf("running version = %+v, want %+v (source constants in version.go)", got, want)
	}
}

func TestParseComponent(t *testing.T) {
	if ParseComponent("") != 0 || ParseComponent("bad") != 0 {
		t.Fatal("empty/invalid must parse to 0")
	}
	if ParseComponent("12") != 12 {
		t.Fatal("expected 12")
	}
}
