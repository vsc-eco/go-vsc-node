package params

import (
	"testing"

	"vsc-node/modules/common/consensusversion"
)

// PinnedVersionFloor must be a pure, deterministic function of config + newEpoch: disabled
// at epoch 0, inactive before the floor epoch, and the configured major.consensus target
// at/after it. The target comes from config (not the running binary) so every node resolves
// the same floor regardless of which version it runs.
func TestPinnedVersionFloor(t *testing.T) {
	target := consensusversion.Version{Major: 0, Consensus: 1}
	cases := []struct {
		name     string
		cp       ConsensusParams
		newEpoch uint64
		want     consensusversion.Version
	}{
		{
			name:     "disabled when floor epoch is 0",
			cp:       ConsensusParams{ConsensusVersionFloorConsensus: 1},
			newEpoch: 1000,
			want:     consensusversion.Version{},
		},
		{
			name:     "inactive before floor epoch",
			cp:       ConsensusParams{ConsensusVersionFloorEpoch: 555, ConsensusVersionFloorConsensus: 1},
			newEpoch: 554,
			want:     consensusversion.Version{},
		},
		{
			name:     "active at floor epoch",
			cp:       ConsensusParams{ConsensusVersionFloorEpoch: 555, ConsensusVersionFloorConsensus: 1},
			newEpoch: 555,
			want:     target,
		},
		{
			name:     "active after floor epoch",
			cp:       ConsensusParams{ConsensusVersionFloorEpoch: 555, ConsensusVersionFloorConsensus: 1},
			newEpoch: 9999,
			want:     target,
		},
		{
			name:     "carries both major and consensus, never non_consensus",
			cp:       ConsensusParams{ConsensusVersionFloorEpoch: 10, ConsensusVersionFloorMajor: 2, ConsensusVersionFloorConsensus: 3},
			newEpoch: 10,
			want:     consensusversion.Version{Major: 2, Consensus: 3},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cp.PinnedVersionFloor(tc.newEpoch); got != tc.want {
				t.Fatalf("PinnedVersionFloor(%d) = %+v, want %+v", tc.newEpoch, got, tc.want)
			}
		})
	}
}

// The pin's purpose: with the floor at 0.1, a pre-pendulum node (0.0.0) is excluded while an
// upgraded node (0.1.0) is admitted. This locks the exclusion semantics the proposer relies on.
func TestPinnedVersionFloorExcludesOldCode(t *testing.T) {
	cp := ConsensusParams{ConsensusVersionFloorEpoch: 555, ConsensusVersionFloorConsensus: 1}
	floor := cp.PinnedVersionFloor(555)

	oldCode := consensusversion.Version{Major: 0, Consensus: 0, NonConsensus: 0}
	newCode := consensusversion.Version{Major: 0, Consensus: 1, NonConsensus: 0}

	if oldCode.MeetsConsensusMin(floor) {
		t.Fatal("pre-pendulum 0.0.0 must NOT meet a 0.1 floor")
	}
	if !newCode.MeetsConsensusMin(floor) {
		t.Fatal("upgraded 0.1.0 must meet a 0.1 floor")
	}
}
