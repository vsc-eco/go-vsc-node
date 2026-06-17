// Package consensusversion defines the triple consensus version major.consensus.non_consensus.
package consensusversion

import (
	"fmt"
	"strconv"
)

// Canonical consensus version of THIS source tree, in the major.consensus.non_consensus
// model. These are the single source of truth for both the on-chain witness announcement
// and TSS gossip readiness.
//
// SOURCE CONSTANTS ON PURPOSE — the version is a property of the code, not of the build
// environment. It is deliberately NOT injected via -ldflags, so every build of this
// source advertises the same triple and the advertised version always equals the version
// the binary actually implements; a build cannot accidentally (or maliciously) claim a
// version it does not run. Bump these in the SAME commit that changes consensus behavior.
//
// Coordination is on Major.Consensus only (the election floor / ConsensusParams);
// NonConsensus is informational and not coordinated.
//
// History:
//   - 0.0.0 — pre-versioning. Binaries without version support announce this implicitly;
//     while the election floor is 0.0, 0.0.0 and newer interoperate.
//   - 0.1.0 — pendulum settlement rollout. The Consensus 0→1 bump is what lets the floor
//     rise to exclude pre-pendulum (0.0.0) nodes from the committee and TSS once a
//     vsc.propose_consensus_version activates (see docs/consensus-upgrades.md).
//   - 0.2.0 — the Consensus 1→2 bump gates THREE independent consensus changes, all
//     activated together when the election floor reaches 0.2.0:
//     (a) try/catch inter-contract calls (ICCallOptions.Try): a caught revert returns a
//     structured outcome + rolls back to a savepoint instead of trapping. Until the
//     floor reaches 0.2.0 the Try flag is IGNORED and a reverting callee traps as
//     before. See TryCatchICCVersion.
//     (b) pendulum LP minimum-floor (B12): the swap-fee split caps the node fraction at
//     BpsScale − MinFractionBps (including on the under-secured cliff), so liquidity
//     providers always retain a minimum share of every pot. Gated on this line via
//     pendulum.LPFloorActivation.
//     (c) consensus delegated stake/unstake (+ delegator pendulum rewards and operator
//     opt-in delegation modes): per-edge delegation accounting so a delegator (not the
//     node operator) can always unstake their own delegated bond; a share-mode node
//     splits its pendulum reward pro-rata to delegators; third-party delegation requires
//     the node to opt in. Gated via StateEngine.delegatedStakeActive. The node's
//     aggregate hive_consensus still feeds election weight + pendulum bond unchanged.
//     Until 0.2.0 is chain-active all three behaviors are inert and splits/call/stake
//     semantics stay byte-identical to 0.1.0, so old and new binaries interoperate until
//     activation.
const (
	currentMajor        uint64 = 0
	currentConsensus    uint64 = 2
	currentNonConsensus uint64 = 0
)

// TryCatchICCVersion is the minimum chain-active consensus version at which the
// try/catch inter-contract-call semantics (ICCallOptions.Try) take effect. Below
// it a Try call behaves exactly like a legacy call (a reverting callee traps the
// caller), so activation is fully coordinated by the election version floor. It
// is part of the v0.2.0 batch, so it is the same line as V0_2_0 (see
// feature_gates.go) — kept as its own named var so the try/catch gate reads by
// FEATURE at its call site.
var TryCatchICCVersion = V0_2_0

// ParseComponent parses a numeric version-component string, defaulting to 0 when
// empty/invalid. Retained for the announcement payload helper; the running version itself
// is no longer parsed from build-time strings (see RunningVersion).
func ParseComponent(raw string) uint64 {
	if raw == "" {
		return 0
	}
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// RunningVersion returns this binary's consensus triple, compiled in from the source
// constants above (not injected via ldflags), so it is identical for every build of this
// source tree.
func RunningVersion() Version {
	return Version{
		Major:        currentMajor,
		Consensus:    currentConsensus,
		NonConsensus: currentNonConsensus,
	}
}

// Version is the canonical on-chain / wire representation.
type Version struct {
	Major        uint64 `json:"major"         bson:"version_major,omitempty"`
	Consensus    uint64 `json:"consensus"     bson:"version_consensus,omitempty"`
	NonConsensus uint64 `json:"non_consensus" bson:"version_non_consensus,omitempty"`
}

// Cmp compares a to b lexicographically (major, then consensus, then non_consensus).
// Returns -1 if a < b, 0 if equal, +1 if a > b.
func (a Version) Cmp(b Version) int {
	if a.Major != b.Major {
		if a.Major < b.Major {
			return -1
		}
		return 1
	}
	if a.Consensus != b.Consensus {
		if a.Consensus < b.Consensus {
			return -1
		}
		return 1
	}
	if a.NonConsensus != b.NonConsensus {
		if a.NonConsensus < b.NonConsensus {
			return -1
		}
		return 1
	}
	return 0
}

// AtLeast returns true if v >= min in the componentwise sense (all fields).
func (v Version) AtLeast(min Version) bool {
	return v.Major >= min.Major && v.Consensus >= min.Consensus && v.NonConsensus >= min.NonConsensus
}

// MeetsConsensusMin returns true if v is compatible with committee / TSS for the given
// chain-adopted minimum (major and consensus must be >= min; non_consensus ignored).
func (v Version) MeetsConsensusMin(min Version) bool {
	return v.Major >= min.Major && v.Consensus >= min.Consensus
}

// Format renders as major.consensus.non_consensus.
func (v Version) Format() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Consensus, v.NonConsensus)
}

// FormatProvisional returns major.(consensus+1).0-p for display during recovery-before-activation,
// using the last adopted triple as the baseline (per protocol spec).
func FormatProvisional(lastAdopted Version) string {
	return fmt.Sprintf("%d.%d.%d-p", lastAdopted.Major, lastAdopted.Consensus+1, 0)
}

// FromLegacy maps the historical single protocol_version field to consensus component.
func FromLegacy(protocolVersion uint64) Version {
	return Version{Major: 0, Consensus: protocolVersion, NonConsensus: 0}
}

// MaxComponentwise returns the componentwise maximum of a and b.
func MaxComponentwise(a, b Version) Version {
	out := a
	if b.Major > out.Major {
		out.Major = b.Major
	}
	if b.Consensus > out.Consensus {
		out.Consensus = b.Consensus
	}
	if b.NonConsensus > out.NonConsensus {
		out.NonConsensus = b.NonConsensus
	}
	return out
}
