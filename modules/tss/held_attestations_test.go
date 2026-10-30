package tss

import (
	"reflect"
	"testing"
)

// The devnet pacemaker compares this view across nodes, so it must list every
// account per target in a stable order and be empty, not nil-panicking, at start.
func TestHeldAttestationsListsAccountsPerTargetSorted(t *testing.T) {
	m := &TssManager{gossipAttestations: map[string]map[string]ReadyAttestation{}}
	if got := m.HeldAttestations(); len(got) != 0 {
		t.Fatalf("fresh manager: %v, want none", got)
	}
	m.gossipAttestations["120"] = map[string]ReadyAttestation{"magi.test3": {}, "magi.test1": {}, "magi.test2": {}}
	m.gossipAttestations["140"] = map[string]ReadyAttestation{"magi.test5": {}}
	want := map[string][]string{"120": {"magi.test1", "magi.test2", "magi.test3"}, "140": {"magi.test5"}}
	if got := m.HeldAttestations(); !reflect.DeepEqual(got, want) {
		t.Fatalf("HeldAttestations() = %v, want %v", got, want)
	}
}
