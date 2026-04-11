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
