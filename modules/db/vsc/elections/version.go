package elections

import "vsc-node/modules/common/consensusversion"

// ResultVersion returns the triple stored in an election result (protocol_version = consensus).
func ResultVersion(r ElectionResult) consensusversion.Version {
	return consensusversion.Version{
		Major:         r.VersionMajor,
		Consensus:     r.ProtocolVersion,
		NonConsensus: r.VersionNonConsensus,
	}
}

// MemberConsensusVersion returns the deterministic version snapshot for TSS gating.
// When HasPerMemberVersion is set, Member* fields come from the witness at election time; otherwise
// legacy elections use the election-level triple for all members (same value for every member).
func MemberConsensusVersion(m ElectionMember, el ElectionResult) consensusversion.Version {
	if m.HasPerMemberVersion {
		return consensusversion.Version{
			Major:         m.MemberMajor,
			Consensus:     m.MemberConsensus,
			NonConsensus: m.MemberNonConsensus,
		}
	}
	return ResultVersion(el)
}
