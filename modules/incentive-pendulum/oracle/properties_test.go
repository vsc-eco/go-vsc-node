package oracle

import (
	"reflect"
	"testing"
)

func TestRunningWitnessGroupSortedAndCapped(t *testing.T) {
	trusted := map[string]bool{
		"w1": true,
		"w2": true,
		"w3": true,
		"w4": false,
	}
	sigs := map[string]int{
		"w1": 8,
		"w2": 10,
		"w3": 10,
		"w4": 100,
	}

	got := RunningWitnessGroup(trusted, sigs, 2)
	want := []string{"w2", "w3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("group=%v want=%v", got, want)
	}
}

func TestHBDAPRModeFromGroup(t *testing.T) {
	group := []string{"w2", "w3", "w1", "w9"}
	props := map[string]WitnessProperties{
		"w1": {HBDInterestRateBps: 1200},
		"w2": {HBDInterestRateBps: 1000},
		"w3": {HBDInterestRateBps: 1000},
		"w9": {HBDInterestRateBps: 1200},
	}

	apr, ok := HBDAPRModeFromGroup(group, props)
	if !ok {
		t.Fatal("expected mode")
	}
	// Tie on count (1000 and 1200 both appear twice) -> conservative lower APR.
	if apr != 1000 {
		t.Fatalf("apr=%d want=1000", apr)
	}
}

func TestHBDAPRModeFromGroupSkipsInvalidRates(t *testing.T) {
	group := []string{"a", "b", "c"}
	props := map[string]WitnessProperties{
		"a": {HBDInterestRateBps: -1},
		"b": {HBDInterestRateBps: MaxHBDInterestRateBps + 1},
	}
	if _, ok := HBDAPRModeFromGroup(group, props); ok {
		t.Fatal("expected no valid mode")
	}

	props["c"] = WitnessProperties{HBDInterestRateBps: 500}
	apr, ok := HBDAPRModeFromGroup(group, props)
	if !ok || apr != 500 {
		t.Fatalf("apr=%d ok=%v", apr, ok)
	}
}
