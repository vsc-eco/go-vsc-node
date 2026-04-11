package consensus_state

import "vsc-node/modules/common/consensusversion"

const singletonID = "singleton"

// ChainConsensusState is persisted chain-global consensus / recovery flags.
type ChainConsensusState struct {
	ID string `bson:"_id"`

	AdoptedVersion consensusversion.Version `bson:"adopted_version"`

	PendingProposal *PendingConsensusProposal `bson:"pending_proposal,omitempty"`

	// ProcessingSuspended blocks normal vsc custom_json processing until cleared by recovery_require_version.
	ProcessingSuspended bool `bson:"processing_suspended"`

	// MinRequiredVersion is set by recovery_require_version; nodes below this must upgrade.
	MinRequiredVersion *consensusversion.Version `bson:"min_required_version,omitempty"`
}

type PendingConsensusProposal struct {
	Major         uint64 `bson:"major"`
	Consensus     uint64 `bson:"consensus"`
	NonConsensus uint64 `bson:"non_consensus"`
	Proposer      string `bson:"proposer"`
	BlockHeight   uint64 `bson:"block_height"`
	TxId          string `bson:"tx_id"`
}

func (p PendingConsensusProposal) Target() consensusversion.Version {
	return consensusversion.Version{
		Major:         p.Major,
		Consensus:     p.Consensus,
		NonConsensus: p.NonConsensus,
	}
}
