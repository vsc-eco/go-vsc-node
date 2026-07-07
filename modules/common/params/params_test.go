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

// VaultRotationV2Enabled must be a pure, deterministic function of config +
// blockHeight: INERT (always false) when the activation height is 0 — the safe
// default on every network today, the property that lets the binary dark-launch
// — and, once pinned, disabled strictly BELOW the height and enabled at/above it
// (mirrors BondInclusionActive; a height at/below an already-processed block
// would diverge live-vs-reindex, hence the strictly-above deploy rule).
func TestVaultRotationV2Enabled(t *testing.T) {
	inert := ConsensusParams{VaultRotationV2ActivationHeight: 0}
	for _, bh := range []uint64{0, 1, 100, 1 << 40} {
		if inert.VaultRotationV2Enabled(bh) {
			t.Fatalf("activation height 0 must be inert, but enabled at bh=%d", bh)
		}
	}

	cp := ConsensusParams{VaultRotationV2ActivationHeight: 100}
	cases := []struct {
		bh   uint64
		want bool
	}{
		{0, false}, {99, false}, {100, true}, {101, true}, {1 << 40, true},
	}
	for _, tc := range cases {
		if got := cp.VaultRotationV2Enabled(tc.bh); got != tc.want {
			t.Fatalf("VaultRotationV2Enabled(bh=%d) with activation=100 = %v, want %v", tc.bh, got, tc.want)
		}
	}
}
