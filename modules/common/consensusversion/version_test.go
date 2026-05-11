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

func TestMergeElectionAndAdoptedMinMajorBump(t *testing.T) {
	prev := Version{Major: 0, Consensus: 3, NonConsensus: 0}
	adopted := Version{Major: 1, Consensus: 0, NonConsensus: 0}
	m := MergeElectionAndAdoptedMin(prev, adopted)
	if m != adopted {
		t.Fatalf("major bump must not inherit old consensus counter: got %+v want %+v", m, adopted)
	}
}
