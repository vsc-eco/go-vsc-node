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
	if se.consensusState == nil {
		return
	}
	st, err := se.consensusState.Get(context.Background())
	if err != nil {
		// Fail CLOSED: a transient consensus-state read error must NOT silently reset
		// the safety flags (BtcKeysignHalted / ProcessingSuspended) to their zero
		// value — retain the last-known cache and retry next block
		// (pruned-methodology F1: halt read-path fail-open).
		return
	}
	se.chainConsensusMu.Lock()
	se.chainConsensusCache = st
	se.chainConsensusMu.Unlock()
}

func (se *StateEngine) chainProcessingSuspended() bool {
	se.chainConsensusMu.RLock()
	defer se.chainConsensusMu.RUnlock()
	return se.chainConsensusCache.ProcessingSuspended
}

// BtcKeysignHalted reports whether the governance multisig has frozen BTC TSS
// keysign via vsc.tss_halt (Build Map §5b). Read by the TSS solvency gate
// through the GetScheduler interface. Mirrors chainProcessingSuspended's cache
// read — deterministic once the halt op has been processed on every node.
func (se *StateEngine) BtcKeysignHalted() bool {
	se.chainConsensusMu.RLock()
	defer se.chainConsensusMu.RUnlock()
	return se.chainConsensusCache.BtcKeysignHalted
}

// BtcTheftHalted reports whether the BTC mapping contract's deterministic theft-halt flag
// ("th", M1.1b SPV-proven auto-trip) is set, mirrored into the consensus cache each block by
// refreshBtcTheftHalt. Read by the TSS solvency gate through GetScheduler — a set flag freezes
// BTC keysign exactly like the governance vsc.tss_halt flag. Deterministic once the tripping
// contract output has been processed on every node.
func (se *StateEngine) BtcTheftHalted() bool {
	se.chainConsensusMu.RLock()
	defer se.chainConsensusMu.RUnlock()
	return se.chainConsensusCache.BtcTheftHalted
}

// refreshBtcTheftHalt mirrors the BTC mapping contract's "th" theft-halt flag into the
// consensus cache each block (M1.1b, Design C — the anti-theft auto-trip's node consumer).
// DETERMINISTIC: every node reads the SAME committed contract output at `height` and writes
// the identical consensus_state. FAIL-SAFE: a transient per-node datalayer/Mongo blip KEEPS
// the last cached value (never resets the safety flag; a rare per-node lag is harmless — the
// keysign gate needs 100% of parties, so a node that missed the halt just stalls the sign,
// never forks). Writes consensus_state only on a CHANGE (no per-block write). The gate then
// reads the mirrored bool with zero I/O — as robust as the governance flag.
func (se *StateEngine) refreshBtcTheftHalt(height uint64) {
	if se.consensusState == nil {
		return
	}
	btcContract := se.sconf.OracleParams().ContractId("BTC")
	if btcContract == "" {
		return // no BTC contract on this network → nothing to mirror
	}
	var transient error
	read := se.btcContractStateReaderAtStrict(btcContract, height, &transient)
	// "th" == btc-mapping-contract constants.BtcTheftHaltKey (a contract-owned key; the node
	// reads it by literal, as it does "s"/"v"). Set by reportUnauthorizedSpend, cleared by
	// clearTheftHalt.
	raw, found := read("th")
	if transient != nil {
		// Per-node datalayer/Mongo blip → KEEP the last value, retry next block (mirrors
		// refreshChainConsensusCache's fail-closed). Never un-halt on a transient error.
		return
	}
	// Any non-empty "th" is a halt. ★ CROSS-REPO LOCK-STEP (M1.1b council L-3): this relies
	// on the contract CLEARING by DELETION (clearTheftHalt → StateDeleteObject("th") → absent
	// → found=false). If the contract ever "cleared" by writing "0"/"false", this would read
	// len>0 and stay HALTED — which fails SAFE (toward the halt, governance-recoverable, never
	// toward signing), but the two repos MUST keep "clear ⇒ key absent."
	halted := found && len(raw) > 0
	se.chainConsensusMu.RLock()
	cur := se.chainConsensusCache.BtcTheftHalted
	se.chainConsensusMu.RUnlock()
	if halted == cur {
		return // unchanged → no consensus_state write
	}
	h := uint64(0)
	if halted {
		h = height
	}
	if err := se.consensusState.SetBtcTheftHalt(context.Background(), halted, h); err != nil {
		// ERROR: this node did NOT apply the theft-halt mirror update; it may keep signing
		// while others freeze (a self-healing stall, not a fork) until a later block succeeds.
		log.Error("M1.1b: SetBtcTheftHalt FAILED — theft-halt mirror not updated on this node",
			"halted", halted, "height", height, "err", err)
		return
	}
	se.refreshChainConsensusCache()
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
