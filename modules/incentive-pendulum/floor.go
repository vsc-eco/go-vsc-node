package pendulum

import "vsc-node/modules/common/consensusversion"

// LP minimum-floor (B12). The PDF split routes an ever-larger share of every
// pendulum pot to nodes as the collateralization ratio s = V/E rises, and the
// full 100% once the under-secured cliff (V ≥ c·E) is crossed — exactly the
// regime a network sits in when consensus stake does not cover pool liquidity.
// The floor caps the node fraction so liquidity providers always retain at
// least MinFractionBps of each pot, including on the cliff.
//
// This is consensus-critical: every node must apply the identical cap, so the
// value is a compile-time constant (baked into the binary, see
// wasm.DefaultConfig) and its activation is gated on a consensus version
// (LPFloorActivation) rather than runtime/governance config.

// DefaultLPFloorBps is the LP minimum-floor baked into wasm.DefaultConfig: the
// minimum fraction (bps; BpsScale = 100%) of every pendulum pot that stays with
// liquidity providers once the floor activates. 2500 bps = 25%, i.e. the node
// share is capped at 75% (BpsScale − DefaultLPFloorBps) even on the under-
// secured cliff.
//
// Economic policy parameter — retune by editing this single constant; it ships
// inert until consensus LPFloorActivation activates, so changing it before
// rollout is free.
const DefaultLPFloorBps int64 = 2_500

// LPFloorActivation is the consensus line (major.consensus) at/after which the
// LP minimum-floor is enforced. Below it the floor is inert and splits are
// byte-identical to the pre-floor 0.1.0 behavior, so pre-upgrade blocks replay
// exactly and 0.1.0/0.2.0 nodes never fork on a swap. Bumped together with the
// source consensus version (consensusversion currentConsensus) in the
// activating commit.
var LPFloorActivation = consensusversion.Version{Major: 0, Consensus: 2}

// MaxNodeShareBps returns the ceiling on the node fraction (bps) implied by an
// LP minimum-floor of minFractionBps: the largest node share that still leaves
// LPs at least minFractionBps of the pot. A minFractionBps <= 0 disables the
// floor (returns BpsScale = no ceiling), so a zero/unset knob is exactly the
// historical PDF behavior; values above BpsScale are clamped to a 100% LP floor
// (node ceiling 0) rather than wrapping.
func MaxNodeShareBps(minFractionBps int64) int64 {
	if minFractionBps <= 0 {
		return BpsScale
	}
	if minFractionBps > BpsScale {
		minFractionBps = BpsScale
	}
	return BpsScale - minFractionBps
}
