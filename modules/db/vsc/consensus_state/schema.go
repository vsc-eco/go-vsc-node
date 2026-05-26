package consensus_state

import "vsc-node/modules/common/consensusversion"

const singletonID = "singleton"

// ChainConsensusState is persisted chain-global recovery state plus the pending
// epoch-scheduled version switch. The *active* consensus version is NOT stored here —
// it is a pure function of the on-chain election (elections.ResultVersion). This record
// only holds (a) the recovery halt flag and (b) the scheduled switch awaiting its
// activation epoch + stake-readiness guard, both consumed deterministically at election build.
type ChainConsensusState struct {
	ID string `bson:"_id"`

	// ProcessingSuspended blocks normal vsc custom_json processing until cleared by recovery_require_version.
	ProcessingSuspended bool `bson:"processing_suspended"`

	// ScheduledActivation is the pending epoch-scheduled version switch (set by
	// vsc.propose_consensus_version, or Forced by recovery_require_version). It is
	// resolved into the election at its activation epoch once the stake-readiness guard passes.
	ScheduledActivation *ScheduledActivation `bson:"scheduled_activation,omitempty"`
}

// ScheduledActivation captures a target major.consensus to switch to at ActivationEpoch.
type ScheduledActivation struct {
	TargetMajor     uint64 `bson:"target_major"`
	TargetConsensus uint64 `bson:"target_consensus"`
	// ActivationEpoch is the election epoch at/after which the switch may activate.
	ActivationEpoch uint64 `bson:"activation_epoch"`
	// Forced skips the stake-readiness guard (recovery path only).
	Forced bool `bson:"forced"`
	// Proposer/TxId/BlockHeight record provenance; BlockHeight makes the read
	// height-addressable (honored only when BlockHeight < query height).
	Proposer    string `bson:"proposer"`
	TxId        string `bson:"tx_id"`
	BlockHeight uint64 `bson:"block_height"`
}

// Target returns the coordinated target (non_consensus is not coordinated).
func (s ScheduledActivation) Target() consensusversion.Version {
	return consensusversion.Version{
		Major:     s.TargetMajor,
		Consensus: s.TargetConsensus,
	}
}
