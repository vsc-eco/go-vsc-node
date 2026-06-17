package state_engine

import (
	"context"
	"strings"

	"vsc-node/modules/common/consensusversion"
	"vsc-node/modules/db/vsc/consensus_state"
	"vsc-node/modules/db/vsc/elections"
)

func isRecoveryAllowlistedCustomJSON(id string) bool {
	switch id {
	case "vsc.recovery_require_version", "vsc.recovery_suspend",
		"vsc.election_result", "vsc.fr_sync":
		return true
	default:
		return false
	}
}

func (se *StateEngine) refreshChainConsensusCache() {
	var next consensus_state.ChainConsensusState
	if se.consensusState != nil {
		if st, err := se.consensusState.Get(context.Background()); err == nil {
			next = st
		}
	}
	se.chainConsensusMu.Lock()
	se.chainConsensusCache = next
	se.chainConsensusMu.Unlock()
}

func (se *StateEngine) chainProcessingSuspended() bool {
	se.chainConsensusMu.RLock()
	defer se.chainConsensusMu.RUnlock()
	return se.chainConsensusCache.ProcessingSuspended
}

// ProcessingSuspendedForPool is used by the transaction pool to reject offchain txs.
func (se *StateEngine) ProcessingSuspendedForPool() bool {
	return se.chainProcessingSuspended()
}

// scheduledActivation returns a copy of the cached pending schedule (or nil).
func (se *StateEngine) scheduledActivation() *consensus_state.ScheduledActivation {
	se.chainConsensusMu.RLock()
	defer se.chainConsensusMu.RUnlock()
	if se.chainConsensusCache.ScheduledActivation == nil {
		return nil
	}
	s := *se.chainConsensusCache.ScheduledActivation
	return &s
}

// ScheduledActivationForHeight returns the pending schedule only if it was recorded before
// blk. This makes the read a pure function of blk so all signers regenerating an election at
// the same height resolve the identical version (and CID), regardless of later proposals.
func (se *StateEngine) ScheduledActivationForHeight(blk uint64) *consensus_state.ScheduledActivation {
	s := se.scheduledActivation()
	if s == nil || s.BlockHeight >= blk {
		return nil
	}
	return s
}

// ActiveConsensusVersion returns the chain-active consensus triple at a block height,
// sourced purely from the on-chain election (deterministic and height-addressable).
func (se *StateEngine) ActiveConsensusVersion(blockHeight uint64) consensusversion.Version {
	elec, err := se.electionDb.GetElectionByHeight(blockHeight)
	if err != nil {
		return consensusversion.Version{}
	}
	return elections.ResultVersion(elec)
}

// delegatedStakeMinVersion is the consensus version at which per-delegator
// consensus stake/unstake activates.
var delegatedStakeMinVersion = consensusversion.Version{Major: 0, Consensus: 2}

// delegatedStakeActive reports whether per-delegator consensus stake/unstake
// semantics are in force at this height (chain-adopted consensus >= 0.2.0).
// Below it, the legacy hive_consensus-holder unstake path runs byte-identically.
// Sourced from ActiveConsensusVersion (on-chain election) so every node agrees.
func (se *StateEngine) delegatedStakeActive(blockHeight uint64) bool {
	return se.ActiveConsensusVersion(blockHeight).AtLeast(delegatedStakeMinVersion)
}

// DelegatedStakeActiveForElection is the same 0.2.0 gate evaluated against an
// already-read election, for the consensus_stake/unstake tx handlers which hold
// an ElectionResult from the StateEngine interface's fail-stop election read.
func DelegatedStakeActiveForElection(elec elections.ElectionResult) bool {
	return elections.ResultVersion(elec).AtLeast(delegatedStakeMinVersion)
}

// TssMinimumConsensusVersion implements the tss.GetScheduler extension: the minimum
// major/consensus triple for TSS at this Hive height (the election-active version).
func (se *StateEngine) TssMinimumConsensusVersion(blockHeight uint64) consensusversion.Version {
	return se.ActiveConsensusVersion(blockHeight)
}

// ElectionMinimumVersion returns the version required for TSS/election participation at this epoch.
func (se *StateEngine) ElectionMinimumVersion(e *elections.ElectionResult) consensusversion.Version {
	if e == nil {
		return consensusversion.Version{}
	}
	return elections.ResultVersion(*e)
}

// DisplayConsensusVersion returns the string to show in APIs: provisional during suspended recovery.
func (se *StateEngine) DisplayConsensusVersion() string {
	active := se.ActiveConsensusVersion(uint64(se.BlockHeight))
	if se.chainProcessingSuspended() {
		return consensusversion.FormatProvisional(active)
	}
	return active.Format()
}

// executeProposeConsensusVersion records an epoch-scheduled version switch. Any committee
// member may propose; a strictly-higher target replaces an existing schedule (monotone, no
// permanent lock). The switch only activates at its epoch once the stake-readiness guard
// passes during election build (see election-proposer GenerateFullElection).
func (se *StateEngine) executeProposeConsensusVersion(tx *TxProposeConsensusVersion) {
	if se.consensusState == nil {
		return
	}
	if tx.NetId != "" && tx.NetId != se.sconf.NetId() {
		return
	}
	elec, err := se.electionDb.GetElectionByHeight(tx.Self.BlockHeight)
	if err != nil {
		return
	}
	proposer := firstHiveAuth(tx.Self.RequiredAuths)
	found := false
	for _, m := range elec.Members {
		if m.Account == proposer {
			found = true
			break
		}
	}
	if !found {
		return
	}
	target := coordinationTarget(consensusversion.Version{Major: tx.Major, Consensus: tx.Consensus})
	// Target must advance beyond the currently active version.
	if target.Cmp(elections.ResultVersion(elec)) <= 0 {
		return
	}
	// Activation epoch: default to the next epoch; reject targets aimed at the past/current.
	activationEpoch := tx.ActivationEpoch
	if activationEpoch == 0 {
		activationEpoch = elec.Epoch + 1
	}
	if activationEpoch <= elec.Epoch {
		return
	}
	// Monotone replace: only a strictly-higher target supersedes an existing schedule.
	if existing := se.scheduledActivation(); existing != nil && target.Cmp(existing.Target()) <= 0 {
		return
	}
	s := &consensus_state.ScheduledActivation{
		TargetMajor:     target.Major,
		TargetConsensus: target.Consensus,
		ActivationEpoch: activationEpoch,
		Forced:          false,
		Proposer:        proposer,
		TxId:            tx.Self.TxId,
		BlockHeight:     tx.Self.BlockHeight,
	}
	if err := se.consensusState.SetScheduledActivation(context.Background(), s); err != nil {
		log.Warn("SetScheduledActivation failed", "err", err)
		return
	}
	se.refreshChainConsensusCache()
}

func (se *StateEngine) executeRecoverySuspend(tx *TxRecoverySuspend) {
	p := se.sconf.ConsensusParams()
	if !VerifyRecoveryMultisig(p, tx.Self.RequiredAuths) {
		// GV-H5: a silent return here hid the fact that on a network whose
		// ConsensusParams omit the recovery roster, the emergency stop is inert.
		// Make the two failure modes distinguishable and loud.
		if !RecoveryMultisigConfigured(p) {
			log.Warn("vsc.recovery_suspend had NO EFFECT: no recovery multisig is configured for this network — "+
				"the on-chain emergency stop is INERT (GV-H5). Populate RecoveryMultisigAccounts and "+
				"RecoveryMultisigThreshold in the network ConsensusParams to enable it.",
				"txId", tx.Self.TxId, "requiredAuths", tx.Self.RequiredAuths)
		} else {
			log.Warn("vsc.recovery_suspend rejected: required_auths do not meet the recovery multisig threshold",
				"txId", tx.Self.TxId, "threshold", p.RecoveryMultisigThreshold, "requiredAuths", tx.Self.RequiredAuths)
		}
		return
	}
	if se.consensusState == nil {
		return
	}
	if err := se.consensusState.SetProcessingSuspended(context.Background(), true); err != nil {
		log.Warn("SetProcessingSuspended failed", "err", err)
		return
	}
	se.refreshChainConsensusCache()
}

// executeRecoveryRequireVersion clears suspension and schedules a Forced switch (skips the
// stake-readiness guard) that activates at the next election epoch (multisig only).
func (se *StateEngine) executeRecoveryRequireVersion(tx *TxRecoveryRequireVersion) {
	p := se.sconf.ConsensusParams()
	if !VerifyRecoveryMultisig(p, tx.Self.RequiredAuths) {
		// GV-H5: see executeRecoverySuspend — surface an unconfigured roster loudly
		// rather than silently dropping the recovery transaction.
		if !RecoveryMultisigConfigured(p) {
			log.Warn("vsc.recovery_require_version had NO EFFECT: no recovery multisig is configured for this network — "+
				"the on-chain emergency stop is INERT (GV-H5). Populate RecoveryMultisigAccounts and "+
				"RecoveryMultisigThreshold in the network ConsensusParams to enable it.",
				"txId", tx.Self.TxId, "requiredAuths", tx.Self.RequiredAuths)
		} else {
			log.Warn("vsc.recovery_require_version rejected: required_auths do not meet the recovery multisig threshold",
				"txId", tx.Self.TxId, "threshold", p.RecoveryMultisigThreshold, "requiredAuths", tx.Self.RequiredAuths)
		}
		return
	}
	if se.consensusState == nil {
		return
	}
	if !se.chainProcessingSuspended() {
		return
	}
	elec, err := se.electionDb.GetElectionByHeight(tx.Self.BlockHeight)
	if err != nil {
		return
	}
	target := coordinationTarget(consensusversion.Version{Major: tx.Major, Consensus: tx.Consensus})
	s := &consensus_state.ScheduledActivation{
		TargetMajor:     target.Major,
		TargetConsensus: target.Consensus,
		ActivationEpoch: elec.Epoch + 1,
		Forced:          true,
		Proposer:        firstHiveAuth(tx.Self.RequiredAuths),
		TxId:            tx.Self.TxId,
		BlockHeight:     tx.Self.BlockHeight,
	}
	if err := se.consensusState.SetForcedActivationAndClearSuspension(context.Background(), s); err != nil {
		log.Warn("SetForcedActivationAndClearSuspension failed", "err", err)
		return
	}
	se.refreshChainConsensusCache()
}

func firstHiveAuth(auths []string) string {
	if len(auths) == 0 {
		return ""
	}
	return strings.TrimPrefix(auths[0], "hive:")
}

// coordinationTarget normalizes a version to the coordinated major.consensus (non_consensus
// is informational and not coordinated).
func coordinationTarget(v consensusversion.Version) consensusversion.Version {
	return consensusversion.Version{Major: v.Major, Consensus: v.Consensus}
}
