package tss

import (
	"testing"

	tss_helpers "vsc-node/modules/tss/helpers"
)

// Item 4: a reshare among fewer parties must not leave a key that less than a
// committee majority can sign. 18 seats: 14 parties (10 signers) proceed, 13
// (9 signers) wait. 12 parties, the BLS-quorum minimum that let 8 colluders sign
// before, are refused.
func TestReshareThresholdFloor(t *testing.T) {
	cases := []struct {
		election, newParties int
		refused              bool
	}{
		{18, 18, false},
		{18, 14, false},
		{18, 13, true},
		{18, 12, true},
		{7, 7, false},
		{7, 5, false},
		{7, 4, true},
		{12, 10, false}, // a genuinely smaller committee lowers the floor
		{12, 9, true},
		{4, 4, false},
		{3, 3, false},
	}
	for _, c := range cases {
		floor := reshareThresholdFloor(c.election)
		newT, _ := tss_helpers.GetThreshold(c.newParties)
		if refused := newT < floor; refused != c.refused {
			t.Errorf("election=%d new=%d: newT=%d floor=%d refused=%v, want %v",
				c.election, c.newParties, newT, floor, refused, c.refused)
		}
		if newT >= floor && newT+1 <= c.election/2 {
			t.Errorf("election=%d new=%d: %d signers is not a committee majority", c.election, c.newParties, newT+1)
		}
		if need := minPartiesForThreshold(floor); (c.newParties < need) != c.refused {
			t.Errorf("election=%d new=%d: minPartiesForThreshold(%d)=%d disagrees with the floor check",
				c.election, c.newParties, floor, need)
		}
	}
}
