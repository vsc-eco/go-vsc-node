package consensus_state

import "vsc-node/modules/common/consensusversion"

const singletonID = "singleton"

// ChainConsensusState is persisted chain-global recovery state plus the bounded set
// of pending consensus-version proposals. The *active* consensus version is NOT stored
// here — it is a pure function of the on-chain election (elections.ResultVersion). This
// record only holds (a) the recovery halt flag, (b) the recovery-multisig forced override,
// and (c) the normal candidate proposals awaiting their activation epoch + stake-readiness
// guard, all consumed deterministically at election build.
type ChainConsensusState struct {
	ID string `bson:"_id"`

	// ProcessingSuspended blocks normal vsc custom_json processing until cleared by recovery_require_version.
	ProcessingSuspended bool `bson:"processing_suspended"`

	// ForcedActivation is the recovery-multisig override (vsc.recovery_require_version):
	// at most one, it bypasses the stake-readiness guard and takes precedence over every
	// normal proposal. The bson key stays "scheduled_activation" for back-compat with any
	// pre-existing on-disk doc.
	ForcedActivation *VersionProposal `bson:"scheduled_activation,omitempty"`

	// VersionProposals is the bounded set of normal candidate version switches (set by
	// vsc.propose_consensus_version), one slot per proposer. The election proposer adopts
	// the highest target the committee is stake-ready for; the election_result handler
	// garbage-collects adopted / expired / no-traction entries.
	VersionProposals []VersionProposal `bson:"version_proposals,omitempty"`
}

// VersionProposal is a single candidate consensus-version switch. This is LOCAL,
// non-hashed state (rebuilt from L1 ops on reindex), so it stores the version as a
// single consensusversion.Version rather than the flat fields the consensus-serialized
// structs keep for wire-format stability. Provenance fields (Proposer/TxId/BlockHeight)
// make reads height-addressable (honored only when BlockHeight < query height) so every
// signer resolves the identical floor → CID.
type VersionProposal struct {
	// Target is the coordinated version to switch to (non_consensus is not coordinated).
	Target consensusversion.Version `bson:"target"`
	// ActivationEpoch is the election epoch at/after which the switch may activate.
	ActivationEpoch uint64 `bson:"activation_epoch"`
	// CreationEpoch is the epoch of the proposing block — anchors the fast-fail window.
	CreationEpoch uint64 `bson:"creation_epoch,omitempty"`
	// ExpiryEpoch is the hard deadline; the proposal is pruned once an election reaches it.
	ExpiryEpoch uint64 `bson:"expiry_epoch,omitempty"`
	// Forced skips the stake-readiness guard (recovery path only).
	Forced bool `bson:"forced"`
	// Proposer/TxId/BlockHeight record provenance; BlockHeight makes the read
	// height-addressable (honored only when BlockHeight < query height).
	Proposer    string `bson:"proposer"`
	TxId        string `bson:"tx_id"`
	BlockHeight uint64 `bson:"block_height"`
}
