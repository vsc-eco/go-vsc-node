package state_engine

import (
	"strings"

	"vsc-node/modules/common/delegationmode"
)

// NodeDelegationMode returns the consensus-delegation mode node `account` has
// opted into, as of blockHeight, normalized to a known value
// (delegationmode.{Deactivated,Share,Custom}). It defaults to Deactivated when
// the node has no announcement at-or-before blockHeight, an unset mode, or the
// witness DB is unavailable — making delegation strict opt-in.
//
// Determinism: the mode comes from the operator's own authenticated Hive
// `update_account` announcement (json_metadata → witness record), which every
// node ingests identically from L1. Reading it at a fixed blockHeight therefore
// yields the same answer on every node, which is required because this gates
// consensus_stake acceptance (here) and the settlement reward split.
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
	return delegationmode.Normalize(w.DelegationMode)
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
