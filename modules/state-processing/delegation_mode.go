package state_engine

import (
	"strings"

	"vsc-node/modules/common/delegationmode"
	"vsc-node/modules/common/params"
	"vsc-node/modules/db/vsc/witnesses"
)

// NodeDelegationMode returns the EFFECTIVE consensus-delegation mode node
// `account` has opted into, as of blockHeight, normalized to a known value
// (delegationmode.{Deactivated,Share,Custom}). It defaults to Deactivated when
// the node has no announcement at-or-before blockHeight, an unset mode, or the
// witness DB is unavailable — making delegation strict opt-in.
//
// "Effective" accounts for the downgrade timelock (consensus 0.5.0+): an adverse
// mode change (leaving Share, which strips delegators' on-chain reward share) is
// deferred until DELEGATION_DOWNGRADE_LOCK_EPOCHS elapse, so this returns the
// prior protected mode until then (see resolveDelegationMode /
// computeDelegationTimelock). Non-adverse changes take effect immediately.
//
// Determinism: the mode comes from the operator's own authenticated Hive
// `update_account` announcement (json_metadata → witness record), which every
// node ingests identically from L1. Reading it at a fixed blockHeight therefore
// yields the same answer on every node, which is required because this gates
// consensus_stake acceptance and the settlement reward split.
//
// Account form: witness records are keyed by the bare Hive account name, while
// callers pass the normalized "hive:" form used throughout the ledger; the
// prefix is stripped before the lookup.
func (se *StateEngine) NodeDelegationMode(account string, blockHeight uint64) string {
	if se == nil || se.witnessDb == nil || account == "" {
		return delegationmode.Default
	}
	bare := strings.TrimPrefix(account, "hive:")
	bh := blockHeight
	w, err := se.witnessDb.GetWitnessAtHeight(bare, &bh)
	if err != nil || w == nil {
		return delegationmode.Default
	}
	return se.resolveDelegationMode(w, blockHeight)
}

// resolveDelegationMode maps a witness row to the mode in force at blockHeight,
// applying the downgrade timelock. A zero DelegationModeMaturityEpoch (pre-0.5.0
// rows and every non-adverse announcement) means "effective immediately", so the
// announced mode is returned unchanged — byte-identical to the pre-feature read.
// Otherwise the announced (adverse) target only takes over once the chain epoch
// reaches the stored maturity; until then the protected DelegationModeEffective
// is returned.
func (se *StateEngine) resolveDelegationMode(w *witnesses.Witness, blockHeight uint64) string {
	target := delegationmode.Normalize(w.DelegationMode)
	if w.DelegationModeMaturityEpoch == 0 {
		return target
	}
	// A pending downgrade exists. Resolve the current epoch from the same
	// fail-stop election read the unbond release uses, so every node agrees or
	// halts (never a silent per-node fork). Below the 0.5.0 gate or pre-genesis,
	// ignore the timelock fields.
	elec, found := se.GetElectionInfoOrBlock(blockHeight)
	if !found || !DelegatedStakeActiveForElection(elec) {
		return target
	}
	if elec.Epoch >= w.DelegationModeMaturityEpoch {
		return target // downgrade matured
	}
	return delegationmode.Normalize(w.DelegationModeEffective) // still protected
}

// computeDelegationTimelock resolves the (effective, maturityEpoch) pair to store
// on a witness announcement row at height Hn for `announced`, enforcing the
// downgrade timelock. It is called at ingest (state engine, before
// SetWitnessUpdate) and returns zero-values (no fields persisted) for pre-0.5.0
// history and for every non-adverse change, keeping those rows byte-clean.
//
// Determinism/replay: reads only on-chain, height-addressable state — the
// election at Hn (fail-stop) and the immediately-preceding witness row (always
// within the 6-record retention window, since it is the newest row < Hn). The
// prior row carries the current effective/pending state forward, so pruning the
// original adverse-announce row never loses it, and re-announcing the same
// pending downgrade carries the original maturity forward WITHOUT resetting it.
func (se *StateEngine) computeDelegationTimelock(account string, Hn uint64, announced string) (string, uint64) {
	target := delegationmode.Normalize(announced)

	// Gate + epoch from one fail-stop election read. Below 0.5.0 (or pre-genesis)
	// store zero-values → readers see the announced mode immediately.
	elec, found := se.GetElectionInfoOrBlock(Hn)
	if !found || !DelegatedStakeActiveForElection(elec) {
		return "", 0
	}
	epoch := elec.Epoch

	bare := strings.TrimPrefix(account, "hive:")
	bh := Hn
	prev, _ := se.witnessDb.GetWitnessAtHeight(bare, &bh) // strict height < Hn → prior row

	// Mode actually in force at Hn (respecting any still-pending prior downgrade).
	effNow := target
	pPending := false
	pTarget := ""
	if prev != nil {
		pTarget = delegationmode.Normalize(prev.DelegationMode)
		if prev.DelegationModeMaturityEpoch != 0 && epoch < prev.DelegationModeMaturityEpoch {
			pPending = true
			effNow = delegationmode.Normalize(prev.DelegationModeEffective) // still-protected mode
		} else {
			effNow = pTarget // prior downgrade matured, or none was pending
		}
	}

	if delegationmode.IsAdverseTransition(effNow, target) {
		if pPending && pTarget == target {
			// Re-announcement of the same still-pending downgrade → carry the
			// original maturity (NO reset), else a periodic re-announce would
			// push the timer out forever.
			return effNow, prev.DelegationModeMaturityEpoch
		}
		// New distinct adverse transition → start the timer from the current epoch.
		return effNow, epoch + params.DELEGATION_DOWNGRADE_LOCK_EPOCHS
	}
	// Non-adverse (→ Share, lateral, or a return to the protected mode): effective
	// immediately, cancelling any pending downgrade. Zero maturity ⇒ no fields
	// persisted and readers return Normalize(DelegationMode) == target.
	return "", 0
}

// PendulumShareDelegations returns, for every committee `member` (in "hive:"
// form) running delegationmode.Share at blockHeight, that node's positive stake
// edges: node -> (delegator -> net HIVE staked). Nodes in any other mode, or
// with no edges, are omitted. The result feeds settlement.ComposeRecord, which
// splits each share node's pendulum slice pro-rata across these edges.
//
// Determinism: edges come from LedgerSystem.AllDelegationEdges (a single ledger
// scan) and modes from the witness DB — both identical on every node at a fixed
// blockHeight. The producer, the apply-time re-derivation, and the structural
// validator MUST all call this with the SAME blockHeight (the settlement's
// SnapshotRangeTo) so they agree byte-for-byte.
//
// Returns ok=false when the edge scan fails transiently, so the producer can
// abort the election attempt and re-derivation can fall back to structural
// validation — never distribute against a partial view.
func (se *StateEngine) PendulumShareDelegations(members []string, blockHeight uint64) (map[string]map[string]int64, bool) {
	if se == nil || se.LedgerSystem == nil {
		return nil, false
	}
	allEdges, ok := se.LedgerSystem.AllDelegationEdges(blockHeight)
	if !ok {
		return nil, false
	}
	out := make(map[string]map[string]int64)
	for _, m := range members {
		if !delegationmode.SharesRewards(se.NodeDelegationMode(m, blockHeight)) {
			continue
		}
		edges := allEdges[m]
		if len(edges) == 0 {
			continue
		}
		// Copy so the returned map never aliases AllDelegationEdges' internal
		// state, and drop any non-positive edge defensively.
		cp := make(map[string]int64, len(edges))
		for d, s := range edges {
			if s > 0 {
				cp[d] = s
			}
		}
		if len(cp) > 0 {
			out[m] = cp
		}
	}
	return out, true
}
