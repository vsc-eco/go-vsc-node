package tss

import (
	"slices"
	"testing"
)

func accountsOf(ps []Participant) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Account)
	}
	slices.Sort(out)
	return out
}

func set(accts ...string) map[string]bool {
	m := make(map[string]bool, len(accts))
	for _, a := range accts {
		m[a] = true
	}
	return m
}

// The testnet stall (epoch 1302): 5 members, tibfox banned, magi.contracts not
// ready, 3 parties left where the commit needs 4. tibfox is ready again, so it
// comes back and the reshare can land.
func TestReadmitBannedResolvesTestnetStall(t *testing.T) {
	members := set("t1", "t2", "t3", "tibfox", "magi.contracts")
	ready := set("t1", "t2", "t3", "tibfox")
	got, back := readmitBanned(
		[]Participant{{Account: "t1"}, {Account: "t2"}, {Account: "t3"}},
		4, []string{"tibfox"}, members, ready, nil, nil)
	if want := []string{"t1", "t2", "t3", "tibfox"}; !slices.Equal(accountsOf(got), want) {
		t.Fatalf("parties = %v, want %v", accountsOf(got), want)
	}
	if !slices.Equal(back, []string{"tibfox"}) {
		t.Fatalf("readmitted = %v, want [tibfox]", back)
	}
}

// Only as many as needed, lowest score first; never one that is not ready,
// blamed on this key, accused, or not eligible for this list.
func TestReadmitBannedTakesOnlyWhatIsNeeded(t *testing.T) {
	selected := []Participant{{Account: "a"}, {Account: "b"}}
	order := []string{"x-notready", "y-blamed", "z-accused", "w-other", "low", "mid", "high"}
	eligible := set("a", "b", "x-notready", "y-blamed", "z-accused", "low", "mid", "high")
	ready := set("a", "b", "y-blamed", "z-accused", "w-other", "low", "mid", "high")
	got, back := readmitBanned(selected, 4, order, eligible, ready, set("y-blamed"), set("z-accused"))
	if !slices.Equal(back, []string{"low", "mid"}) {
		t.Fatalf("readmitted = %v, want [low mid]", back)
	}
	if len(got) != 4 {
		t.Fatalf("got %d parties, want 4", len(got))
	}
}

// Enough parties already: nothing changes.
func TestReadmitBannedNoopWhenEnough(t *testing.T) {
	selected := []Participant{{Account: "a"}, {Account: "b"}, {Account: "c"}}
	got, back := readmitBanned(selected, 3, []string{"d"}, set("d"), set("d"), nil, nil)
	if back != nil || len(got) != 3 {
		t.Fatalf("readmitted %v with enough parties", back)
	}
}
