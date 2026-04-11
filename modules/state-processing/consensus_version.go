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
		"vsc.election_result", "vsc.fr_sync", "vsc.actions":
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
	target := st.PendingProposal.Target()

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

	blocksSince := uint64(0)
	if blockHeight > prevElec.BlockHeight {
		blocksSince = blockHeight - prevElec.BlockHeight
	}
	minVotes := elections.MinimalRequiredElectionVotes(blocksSince, totalWeight)

	var readyWeight uint64
	for i, m := range prevElec.Members {
		w, err := se.witnessDb.GetWitnessAtHeight(m.Account, &blockHeight)
		if err != nil || w == nil {
			continue
		}
		if !w.ConsensusVersionTriple().MeetsConsensusMin(target) {
			continue
		}
		if len(prevElec.Weights) == 0 {
			readyWeight++
		} else if i < len(prevElec.Weights) {
			readyWeight += prevElec.Weights[i]
		}
	}

	if readyWeight >= minVotes {
		_ = se.consensusState.SetAdoptedVersion(context.Background(), target)
		_ = se.consensusState.ClearPendingProposal(context.Background())
		se.refreshChainConsensusCache()
	}
}

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
		if m.Account == proposer || firstHiveAuth([]string{m.Account}) == proposer {
			found = true
			break
		}
	}
	if !found {
		return
	}
	prop := &consensus_state.PendingConsensusProposal{
		Major:         tx.Major,
		Consensus:     tx.Consensus,
		NonConsensus: tx.NonConsensus,
		Proposer:      firstHiveAuth(tx.Self.RequiredAuths),
		BlockHeight:   tx.Self.BlockHeight,
		TxId:          tx.Self.TxId,
	}
	_ = se.consensusState.SetPendingProposal(context.Background(), prop)
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
	_ = se.consensusState.SetProcessingSuspended(context.Background(), true)
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
	v := consensusversion.Version{
		Major:         tx.Major,
		Consensus:     tx.Consensus,
		NonConsensus: tx.NonConsensus,
	}
	_ = se.consensusState.SetMinRequiredAndClearSuspension(context.Background(), v)
	se.refreshChainConsensusCache()
}

func firstHiveAuth(auths []string) string {
	if len(auths) == 0 {
		return ""
	}
	return strings.TrimPrefix(auths[0], "hive:")
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
	return consensusversion.MaxComponentwise(v, adopted)
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
