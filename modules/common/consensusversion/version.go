// Package consensusversion defines the triple consensus version major.consensus.non_consensus.
package consensusversion

import (
	"fmt"
)

// Version is the canonical on-chain / wire representation.
type Version struct {
	Major         uint64 `json:"major" bson:"version_major,omitempty"`
	Consensus     uint64 `json:"consensus" bson:"version_consensus,omitempty"`
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
