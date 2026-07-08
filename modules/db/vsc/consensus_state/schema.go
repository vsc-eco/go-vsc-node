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

	// OutboundHalts is the set of active outbound-halt entries (v0.6.0 vault
	// protections). The gateway OUTBOUND path (withdrawals to L1) is frozen at
	// height H if any entry is active at H — i.e. SetHeight <= H < ExpiryHeight.
	// Entries are added by the single-node vsc.halt op (one committee member,
	// bounded window, additional signers stack to extend) and by the solvency
	// auto-halt effect; the height-bounded ExpiryHeight makes every halt
	// auto-expire deterministically without an explicit un-halt op. This is a
	// SEPARATE, lighter primitive from ProcessingSuspended above: that one is
	// recovery-multisig-only and blocks INBOUND settlement; this one is
	// single-node / auto and blocks only OUTBOUND payouts. Distinct so a routine
	// outbound freeze never has to touch the heavy recovery halt.
	OutboundHalts []OutboundHalt `bson:"outbound_halts,omitempty"`
}

// OutboundHalt is a single active freeze on the gateway outbound path. It is
// height-addressable (active iff SetHeight <= queryHeight < ExpiryHeight) so
// every signer resolves the identical halt verdict → identical batch → CID at a
// given tick, and expiry is deterministic (no wall-clock, no explicit un-halt).
type OutboundHalt struct {
	// Account is the halt's setter: a current committee member for a vsc.halt op,
	// or the reserved "system" sentinel for the solvency auto-halt effect. Used to
	// enforce per-node once-per-period anti-griefing and to attribute the freeze.
	Account string `bson:"account"`
	// Reason is a short free-text tag for operators (not consensus-significant
	// beyond being part of the stored record).
	Reason string `bson:"reason,omitempty"`
	// SetHeight is the L1 block height at which the halt was set (makes the read
	// height-addressable: honored only when SetHeight <= query height).
	SetHeight uint64 `bson:"set_height"`
	// ExpiryHeight is the first height at which this entry is no longer active.
	// The bounded window (ExpiryHeight - SetHeight) is capped in the handler so a
	// single node cannot freeze outbounds indefinitely.
	ExpiryHeight uint64 `bson:"expiry_height"`
	// TxId records provenance of the op that set the halt.
	TxId string `bson:"tx_id,omitempty"`
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
