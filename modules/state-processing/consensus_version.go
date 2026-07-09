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

// forcedActivation returns a copy of the cached recovery override (or nil).
func (se *StateEngine) forcedActivation() *consensus_state.VersionProposal {
	se.chainConsensusMu.RLock()
	defer se.chainConsensusMu.RUnlock()
	if se.chainConsensusCache.ForcedActivation == nil {
		return nil
	}
	s := *se.chainConsensusCache.ForcedActivation
	return &s
}

// versionProposals returns a copy of the cached normal-proposal set.
func (se *StateEngine) versionProposals() []consensus_state.VersionProposal {
	se.chainConsensusMu.RLock()
	defer se.chainConsensusMu.RUnlock()
	if len(se.chainConsensusCache.VersionProposals) == 0 {
		return nil
	}
	out := make([]consensus_state.VersionProposal, len(se.chainConsensusCache.VersionProposals))
	copy(out, se.chainConsensusCache.VersionProposals)
	return out
}

// ForcedActivationForHeight returns the recovery override only if it was recorded
// before blk. Height-addressable so all signers resolve the identical floor → CID.
func (se *StateEngine) ForcedActivationForHeight(blk uint64) *consensus_state.VersionProposal {
	s := se.forcedActivation()
	if s == nil || s.BlockHeight >= blk {
		return nil
	}
	return s
}

// VersionProposalsForHeight returns the candidate proposals recorded before blk
// (height-addressable). Expiry is NOT applied here — the election builder evaluates
// liveness against the election epoch deterministically.
func (se *StateEngine) VersionProposalsForHeight(blk uint64) []consensus_state.VersionProposal {
	all := se.versionProposals()
	out := all[:0]
	for _, p := range all {
		if p.BlockHeight < blk {
			out = append(out, p)
		}
	}
	return out
}

// upsertVersionProposal writes p into the proposal set keyed by proposer (one slot
// each, replacing any prior entry from the same proposer) and persists. The state
// engine processes blocks serially, so this read-modify-write is race-free.
func (se *StateEngine) upsertVersionProposal(p consensus_state.VersionProposal) {
	props := se.versionProposals()
	replaced := false
	for i := range props {
		if props[i].Proposer == p.Proposer {
			props[i] = p
			replaced = true
			break
		}
	}
	if !replaced {
		props = append(props, p)
	}
	if err := se.consensusState.SetVersionProposals(context.Background(), props); err != nil {
		log.Warn("SetVersionProposals failed", "err", err)
		return
	}
	se.refreshChainConsensusCache()
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

// delegatedStakeActive reports whether per-delegator consensus stake/unstake
// semantics are in force at this height (the v0.5.0 batch — see
// consensusversion.DelegatedStakeActive). Below the line the legacy
// hive_consensus-holder unstake path runs byte-identically. Sourced from
// ActiveConsensusVersion (on-chain election) so every node agrees.
func (se *StateEngine) delegatedStakeActive(blockHeight uint64) bool {
	return consensusversion.DelegatedStakeActive(se.ActiveConsensusVersion(blockHeight))
}

// DelegatedStakeActiveForElection is the same 0.5.0 gate evaluated against an
// already-read election, for the consensus_stake/unstake tx handlers which hold
// an ElectionResult from the StateEngine interface's fail-stop election read.
func DelegatedStakeActiveForElection(elec elections.ElectionResult) bool {
	return consensusversion.DelegatedStakeActive(elections.ResultVersion(elec))
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
	if proposer == "" {
		return
	}
	// Proposer must be a current committee member. Normalize the "hive:" prefix on
	// both sides so a prefixed member still matches the bare auth account.
	found := false
	for _, m := range elec.Members {
		if strings.TrimPrefix(m.Account, "hive:") == proposer {
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
	// Self-version gate: a proposer may only propose up to the version it is
	// ANNOUNCING (i.e. running) — "upgrade your node before proposing it to the
	// network." Read the proposer's witness record at this height; reject if its
	// announced triple does not meet the target. Deterministic (on-chain witness
	// record at a fixed height, identical on every node). A read error drops the op
	// (like the election read above); a missing record skips the gate — a real
	// committee member always has an announcement, so this only relaxes test paths.
	if se.witnessDb != nil {
		bh := tx.Self.BlockHeight
		w, werr := se.witnessDb.GetWitnessAtHeight(proposer, &bh)
		if werr != nil {
			return
		}
		if w != nil && !w.ConsensusVersionTriple().MeetsConsensusMin(target) {
			return
		}
	}
	// Activation epoch: default to the next epoch; reject targets aimed at the past/current.
	activationEpoch := tx.ActivationEpoch
	if activationEpoch == 0 {
		activationEpoch = elec.Epoch + 1
	}
	if activationEpoch <= elec.Epoch {
		return
	}
	// One candidate slot per proposer (re-proposing replaces your own). There is no
	// "strictly higher than the standing schedule" rule, so a high junk target can no
	// longer block a legitimate lower one — each candidate is adopted (or not) by the
	// readiness guard on its own merits, and garbage-collected at expiry / no-traction.
	se.upsertVersionProposal(consensus_state.VersionProposal{
		Target:          target,
		ActivationEpoch: activationEpoch,
		CreationEpoch:   elec.Epoch,
		ExpiryEpoch:     elec.Epoch + se.sconf.ConsensusParams().VersionProposalExpiry(),
		Forced:          false,
		Proposer:        proposer,
		TxId:            tx.Self.TxId,
		BlockHeight:     tx.Self.BlockHeight,
	})
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
	s := &consensus_state.VersionProposal{
		Target:          target,
		ActivationEpoch: elec.Epoch + 1,
		CreationEpoch:   elec.Epoch,
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

// PruneVersionProposalsAfterElection garbage-collects the candidate set + recovery
// override once a ratified election makes a new floor canonical. It drops proposals
// that are adopted/sub-floor (target <= floor), past their hard deadline
// (expiryEpoch <= epoch), or have no traction beyond their own proposer past the
// fast-fail window. Deterministic: it reads only the ratified election (members,
// weights, per-member announced versions) + the proposal set, so every node prunes
// identically. This is the wiring that replaces the never-called ClearScheduledActivation.
func (se *StateEngine) PruneVersionProposalsAfterElection(elec elections.ElectionResult) {
	if se.consensusState == nil {
		return
	}
	floor := elections.ResultVersion(elec)
	epoch := elec.Epoch
	fastFail := se.sconf.ConsensusParams().VersionProposalFastFail()

	// Clear the recovery override once it has been adopted (target <= floor).
	if f := se.forcedActivation(); f != nil && f.Target.Cmp(floor) <= 0 {
		if err := se.consensusState.SetForcedActivation(context.Background(), nil); err != nil {
			log.Warn("clear forced activation failed", "err", err)
		} else {
			se.refreshChainConsensusCache()
		}
	}

	props := se.versionProposals()
	if len(props) == 0 {
		return
	}
	kept := props[:0]
	changed := false
	for _, p := range props {
		drop := false
		switch {
		case p.Target.Cmp(floor) <= 0: // adopted or sub-floor → auto-cancel
			drop = true
		case p.ExpiryEpoch != 0 && epoch >= p.ExpiryEpoch: // hard deadline
			drop = true
		case fastFail > 0 && epoch >= p.CreationEpoch+fastFail &&
			versionProposalReadyWeight(elec, p.Target) <= electionMemberWeightOf(elec, p.Proposer):
			drop = true // no traction beyond the proposer
		}
		if drop {
			changed = true
			continue
		}
		kept = append(kept, p)
	}
	if !changed {
		return
	}
	if err := se.consensusState.SetVersionProposals(context.Background(), kept); err != nil {
		log.Warn("prune version proposals failed", "err", err)
		return
	}
	se.refreshChainConsensusCache()
}

// versionProposalReadyWeight sums the election weight of members whose announced
// version meets target — the same readiness basis the election proposer uses.
func versionProposalReadyWeight(elec elections.ElectionResult, target consensusversion.Version) uint64 {
	var total uint64
	for i, m := range elec.Members {
		if elections.MemberConsensusVersion(m, elec).MeetsConsensusMin(target) {
			total += electionWeightAt(elec, i)
		}
	}
	return total
}

// electionMemberWeightOf returns account's election weight (0 if not a member).
func electionMemberWeightOf(elec elections.ElectionResult, account string) uint64 {
	acct := strings.TrimPrefix(account, "hive:")
	for i, m := range elec.Members {
		if strings.TrimPrefix(m.Account, "hive:") == acct {
			return electionWeightAt(elec, i)
		}
	}
	return 0
}

// electionWeightAt returns Weights[i], or 1 for an unweighted election.
func electionWeightAt(elec elections.ElectionResult, i int) uint64 {
	if len(elec.Weights) == len(elec.Members) && i < len(elec.Weights) {
		return elec.Weights[i]
	}
	return 1
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
