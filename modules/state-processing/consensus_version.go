package state_engine

import (
	"context"
	"strings"

	"vsc-node/modules/common/consensusversion"
	"vsc-node/modules/db/vsc/consensus_state"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/witnesses"
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
		se.chainConsensusCache = consensus_state.ChainConsensusState{}
		return
	}
	st, err := se.consensusState.Get(context.Background())
	if err != nil {
		se.chainConsensusCache = consensus_state.ChainConsensusState{}
		return
	}
	se.chainConsensusCache = st
}

func (se *StateEngine) chainProcessingSuspended() bool {
	return se.chainConsensusCache.ProcessingSuspended
}

// EffectiveAdoptedConsensusVersion returns chain-global adopted version (from DB).
func (se *StateEngine) EffectiveAdoptedConsensusVersion() consensusversion.Version {
	if se.consensusState == nil {
		return consensusversion.Version{}
	}
	return se.chainConsensusCache.AdoptedVersion
}

// DisplayConsensusVersion returns the string to show in APIs: provisional during suspended recovery.
func (se *StateEngine) DisplayConsensusVersion() string {
	adopted := se.EffectiveAdoptedConsensusVersion()
	if se.chainProcessingSuspended() {
		return consensusversion.FormatProvisional(adopted)
	}
	return adopted.Format()
}

// TryFinalizeConsensusProposal checks stake-weighted witness readiness for a pending target version.
func (se *StateEngine) TryFinalizeConsensusProposal(blockHeight uint64) {
	if se.consensusState == nil {
		return
	}
	st := se.chainConsensusCache
	if st.PendingProposal == nil {
		return
	}
	target := coordinationTarget(st.PendingProposal.Target())
	adopted := coordinationTarget(se.EffectiveAdoptedConsensusVersion())
	if target.Cmp(adopted) < 0 {
		log.Warn("pending consensus target is below adopted; refusing finalize", "target", target.Format(), "adopted", adopted.Format())
		return
	}

	prevElec, err := se.electionDb.GetElectionByHeight(blockHeight)
	if err != nil {
		return
	}

	var totalWeight uint64
	if len(prevElec.Weights) == 0 {
		totalWeight = uint64(len(prevElec.Members))
	} else {
		for _, w := range prevElec.Weights {
			totalWeight += w
		}
	}
	if totalWeight == 0 {
		log.Warn("cannot finalize consensus proposal with zero election weight", "blockHeight", blockHeight)
		return
	}

	minVotes := elections.MinimalRequiredConsensusVersionVotes(totalWeight)

	var readyWeight uint64
	for i, m := range prevElec.Members {
		ready := false
		// Deterministic fast path: for elections that snapshot per-member versions,
		// finalize readiness must come from election data, not local witness DB state.
		if m.HasPerMemberVersion {
			mv := coordinationTarget(elections.MemberConsensusVersion(m, prevElec))
			ready = mv.MeetsConsensusMin(target)
		} else {
			// Legacy elections fallback.
			w, err := se.witnessDb.GetWitnessAtHeight(m.Account, &blockHeight)
			if err == nil && w != nil {
				ready = coordinationTarget(w.ConsensusVersionTriple()).MeetsConsensusMin(target)
			}
		}
		if !ready {
			continue
		}
		if len(prevElec.Weights) == 0 {
			readyWeight++
		} else if i < len(prevElec.Weights) {
			readyWeight += prevElec.Weights[i]
		}
	}

	if readyWeight >= minVotes {
		if err := se.consensusState.SetAdoptedVersion(context.Background(), target); err != nil {
			log.Warn("SetAdoptedVersion failed", "err", err)
			return
		}
		if err := se.consensusState.ClearPendingProposal(context.Background()); err != nil {
			log.Warn("ClearPendingProposal failed after adopt", "err", err)
			return
		}
		if st.PendingProposal != nil {
			act := &consensus_state.ConsensusActivation{
				Mode:                "normal",
				Version:             target,
				ActivationHeight:    blockHeight + 1,
				AttestedBlockHeight: blockHeight,
				AttestedTxId:        st.PendingProposal.TxId,
			}
			if err := se.consensusState.SetNextActivation(context.Background(), act); err != nil {
				log.Warn("SetNextActivation failed after adopt", "err", err)
				return
			}
		}
		se.refreshChainConsensusCache()
	}
}

func (se *StateEngine) executeProposeConsensusVersion(tx *TxProposeConsensusVersion) {
	if se.consensusState == nil {
		return
	}
	st := se.chainConsensusCache
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
		if m.Account == proposer || firstHiveAuth([]string{m.Account}) == proposer {
			found = true
			break
		}
	}
	if !found {
		return
	}
	target := coordinationTarget(consensusversion.Version{
		Major:         tx.Major,
		Consensus:     tx.Consensus,
		NonConsensus: tx.NonConsensus,
	})
	adopted := coordinationTarget(se.EffectiveAdoptedConsensusVersion())
	if target.Cmp(adopted) < 0 {
		return
	}
	if st.PendingProposal != nil {
		pending := coordinationTarget(st.PendingProposal.Target())
		// Coordination mode: once a target is pending, no different target can
		// replace it until it is adopted/cleared.
		if target.Cmp(pending) != 0 {
			return
		}
		if target.Cmp(pending) == 0 {
			se.TryFinalizeConsensusProposal(tx.Self.BlockHeight)
			return
		}
	}
	prop := &consensus_state.PendingConsensusProposal{
		Major:         tx.Major,
		Consensus:     tx.Consensus,
		NonConsensus: 0,
		Proposer:      firstHiveAuth(tx.Self.RequiredAuths),
		BlockHeight:   tx.Self.BlockHeight,
		TxId:          tx.Self.TxId,
	}
	if err := se.consensusState.SetPendingProposal(context.Background(), prop); err != nil {
		log.Warn("SetPendingProposal failed", "err", err)
		return
	}
	se.refreshChainConsensusCache()
	se.TryFinalizeConsensusProposal(tx.Self.BlockHeight)
}

func (se *StateEngine) executeRecoverySuspend(tx *TxRecoverySuspend) {
	p := se.sconf.ConsensusParams()
	if !VerifyRecoveryMultisig(p, tx.Self.RequiredAuths) {
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

func (se *StateEngine) executeRecoveryRequireVersion(tx *TxRecoveryRequireVersion) {
	p := se.sconf.ConsensusParams()
	if !VerifyRecoveryMultisig(p, tx.Self.RequiredAuths) {
		return
	}
	if se.consensusState == nil {
		return
	}
	if !se.chainConsensusCache.ProcessingSuspended {
		return
	}
	v := consensusversion.Version{
		Major:         tx.Major,
		Consensus:     tx.Consensus,
		NonConsensus: tx.NonConsensus,
	}
	if err := se.consensusState.SetMinRequiredAndClearSuspension(context.Background(), v); err != nil {
		log.Warn("SetMinRequiredAndClearSuspension failed", "err", err)
		return
	}
	act := &consensus_state.ConsensusActivation{
		Mode:                "recovery",
		Version:             coordinationTarget(v),
		ActivationHeight:    tx.Self.BlockHeight + 1,
		AttestedBlockHeight: tx.Self.BlockHeight,
		AttestedTxId:        tx.Self.TxId,
	}
	if err := se.consensusState.SetNextActivation(context.Background(), act); err != nil {
		log.Warn("SetNextActivation failed for recovery", "err", err)
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

func coordinationTarget(v consensusversion.Version) consensusversion.Version {
	return consensusversion.Version{
		Major:         v.Major,
		Consensus:     v.Consensus,
		NonConsensus: 0,
	}
}

// WitnessMeetsEffectiveMinimum returns true if the witness can participate in TSS for the given election minimum.
func WitnessMeetsEffectiveMinimum(w *witnesses.Witness, min consensusversion.Version) bool {
	if w == nil {
		return false
	}
	return w.ConsensusVersionTriple().MeetsConsensusMin(min)
}

// ElectionMinimumVersion returns the minimum triple required for TSS/election participation at this epoch.
func (se *StateEngine) ElectionMinimumVersion(e *elections.ElectionResult) consensusversion.Version {
	if e == nil {
		return consensusversion.Version{}
	}
	v := elections.ResultVersion(*e)
	adopted := se.EffectiveAdoptedConsensusVersion()
	return consensusversion.MergeElectionAndAdoptedMin(v, adopted)
}

// ProcessingSuspendedForPool is used by the transaction pool to reject offchain txs.
func (se *StateEngine) ProcessingSuspendedForPool() bool {
	return se.chainProcessingSuspended()
}

// TssMinimumConsensusVersion implements tss.GetScheduler extension.
func (se *StateEngine) TssMinimumConsensusVersion(blockHeight uint64) consensusversion.Version {
	elec, err := se.electionDb.GetElectionByHeight(blockHeight)
	if err != nil {
		return consensusversion.Version{}
	}
	return se.ElectionMinimumVersion(&elec)
}
