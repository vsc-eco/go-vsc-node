package tss

import (
	"fmt"
	"testing"

	"vsc-node/modules/common/consensusversion"
	tss_helpers "vsc-node/modules/tss/helpers"

	"github.com/stretchr/testify/assert"
)

// P8 (testnet 2026-10-22): in a 4-member committee one landed accusation left the
// accused out of every reshare of the key; 3 parties lower the threshold below the
// Item-4 floor, so every attempt was refused, and only a successful reshare would
// have cleared the accusation. From 0.12.0 the exclusion yields to the same minimum
// the ban-yield uses.

var (
	p8Before = consensusversion.V0_11_0
	p8After  = consensusversion.V0_12_0
)

func p8Members(n int) []Participant {
	out := make([]Participant, n)
	for i := range out {
		out[i] = Participant{Account: fmt.Sprintf("m%02d", i)}
	}
	return out
}

// floorRefuses replays the call site's floor check (tss.go "reshare would lower
// the key threshold") for a new committee of newSize in an election of n.
func floorRefuses(active consensusversion.Version, n, newSize int) bool {
	if !consensusversion.TssReshareKeepsThresholdActive(active) {
		return false
	}
	newThr, _ := tss_helpers.GetThreshold(newSize)
	return newThr < reshareThresholdFloor(n)
}

func TestP8_AccuseNewMin(t *testing.T) {
	assert.Equal(t, 0, accuseNewMin(p8Before, 4), "inert before 0.12.0")
	for n, want := range map[int]int{3: 2, 4: 4, 5: 4, 7: 5, 19: 14} {
		assert.Equal(t, want, accuseNewMin(p8After, n), "n=%d", n)
	}
}

// The live case end to end through the selection the call site runs.
func TestP8_FourMemberCommitteeAccusationNoLongerBlocksTheReshare(t *testing.T) {
	members := p8Members(4)
	counts := map[string]int{"m03": 1} // the accused (tibfox on testnet)
	_, keepNew, out := applyAccusedExclusions(members, members, counts, 4, 0, accuseNewMin(p8Before, 4), consensusversion.TssAccuseStackActive(p8Before))
	assert.True(t, out["m03"], "0.11: the accused is left out")
	assert.True(t, floorRefuses(p8Before, 4, len(keepNew)), "0.11: and the floor refuses the reshare (the wedge)")

	_, keepNew, out = applyAccusedExclusions(members, members, counts, 4, 0, accuseNewMin(p8After, 4), consensusversion.TssAccuseStackActive(p8After))
	assert.Empty(t, out, "0.12: the exclusion yields, nobody is left out")
	assert.Len(t, keepNew, 4)
	assert.False(t, floorRefuses(p8After, 4, len(keepNew)), "0.12: the reshare can run")
}

// Where one exclusion fits, it still happens: the yield only fires when it must.
func TestP8_ExclusionStillAppliesWhereItFits(t *testing.T) {
	for _, n := range []int{3, 5, 6, 7, 13, 19} {
		members := p8Members(n)
		counts := map[string]int{members[n-1].Account: 1}
		_, keepNew, out := applyAccusedExclusions(members, members, counts, n, 0, accuseNewMin(p8After, n), consensusversion.TssAccuseStackActive(p8After))
		assert.True(t, out[members[n-1].Account], "n=%d: the accused is still left out", n)
		assert.Len(t, keepNew, n-1, "n=%d", n)
		assert.False(t, floorRefuses(p8After, n, len(keepNew)), "n=%d: and the reshare is not refused", n)
	}
}

// The other rules are unchanged by a zero minimum (before 0.12.0).
func TestP8_ZeroMinimumKeepsTheOldBehaviour(t *testing.T) {
	members := p8Members(4)
	counts := map[string]int{"m03": 2}
	_, keepNew, out := applyAccusedExclusions(members, members, counts, 4, 0, 0, false)
	assert.Equal(t, map[string]bool{"m03": true}, out)
	assert.Len(t, keepNew, 3)
	_, _, out = applyAccusedExclusions(members, members, counts, 4, 1, accuseNewMin(p8After, 4), consensusversion.TssAccuseStackActive(p8After))
	assert.Empty(t, out, "blame/ban already left someone out: POA-8 adds nothing (unchanged)")
}
