package ledgerSystem

import ledgerDb "vsc-node/modules/db/vsc/ledger"

// groupDelegationEdges is the pure core of AllDelegationEdges: it folds raw
// consensus stake/unstake ledger records into node -> (delegator -> net stake),
// keeping only strictly-positive net edges. Only AssetDelegation rows
// contribute; the gross AssetDelegationTotal rows and the plain hive /
// hive_consensus legs are ignored. Order-independent (summation), so the result
// is identical regardless of record order — the determinism the settlement
// reward split relies on.
func groupDelegationEdges(records []ledgerDb.LedgerRecord) map[string]map[string]int64 {
	net := map[string]int64{}
	for _, r := range records {
		if r.Asset != AssetDelegation {
			continue
		}
		net[r.Owner] += r.Amount
	}

	out := make(map[string]map[string]int64)
	for key, amt := range net {
		if amt <= 0 {
			continue
		}
		from, to := splitDelegationEdgeKey(key)
		if from == "" || to == "" {
			continue
		}
		edges := out[to]
		if edges == nil {
			edges = make(map[string]int64)
			out[to] = edges
		}
		edges[from] = amt
	}
	return out
}

// AllDelegationEdges returns every node's positive consensus-delegation edges as
// of blockHeight: node account -> (delegator account -> net HIVE delegated on
// that edge). Both keys are in the "hive:" form the stake ops use; the
// operator's own self-stake appears as the edge node -> node.
//
// It is built from a single scan of the consensus stake/unstake ledger (the
// same canonical source the 0.3.0 migration pairs edges from), summing the
// virtual AssetDelegation rows per composite owner. Because that ledger is
// identical on every node and summation is order-independent, two honest nodes
// at the same height produce a byte-identical map — a hard requirement, since
// this drives the pendulum reward split baked into the attested settlement
// record. Only strictly-positive net edges are kept (a fully-unstaked edge is
// dropped).
//
// Returns ok=false on a transient ledger read error so callers fail-stop (the
// settlement producer aborts; re-derivation falls back to structural checks)
// rather than distribute rewards against a partial view.
func (ls *ledgerSystem) AllDelegationEdges(blockHeight uint64) (map[string]map[string]int64, bool) {
	if ls.LedgerDb == nil {
		return nil, false
	}
	records, err := ls.LedgerDb.GetLedgerRecordsByType(
		[]string{"consensus_stake", "consensus_unstake"}, blockHeight)
	if err != nil {
		return nil, false
	}
	return groupDelegationEdges(records), true
}
