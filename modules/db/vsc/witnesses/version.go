package witnesses

import "vsc-node/modules/common/consensusversion"

// ConsensusVersionTriple returns the announced triple; protocol_version maps to consensus.
func (w Witness) ConsensusVersionTriple() consensusversion.Version {
	return consensusversion.Version{
		Major:         w.VersionMajor,
		Consensus:     w.ProtocolVersion,
		NonConsensus: w.VersionNonConsensus,
	}
}
