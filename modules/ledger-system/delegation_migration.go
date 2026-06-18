package ledgerSystem

import (
	"sort"
	"strings"

	ledgerDb "vsc-node/modules/db/vsc/ledger"
)

// BackfillDelegationEdges derives the per-edge delegation ledger records to seed
// at the consensus 0.2.0 activation height, from the full history of consensus
// stake/unstake records. This is what makes pre-0.2.0 stakes reclaimable by the
// delegator who made them.
//
// Why pairing is needed: before 0.2.0 the (from -> to) link is not stored on any
// single record — LedgerUpdate/LedgerRecord has no destination field. A stake
// emits a "#in" row (Owner = from, -amount, hive) and a "#out" row (Owner = to,
// +amount, hive_consensus) sharing one base id. We recover (from, to, amount) by
// pairing those two rows on their base id (id with the trailing "#..." stripped).
//
// Legacy unstakes (< 0.2.0) were only ever issued by the bond holder (from == to
// == node), so they reduce that node's self-edge.
//
// Determinism: the input is canonical ledger state (identical on every node) and
// the output is sorted by edge key, so every node seeds byte-identical edges.
// Idempotency is the caller's responsibility (run exactly once, guarded by an
// activation marker — see the wiring TODO in the design spec).
//
// Returns one net "delegation" record per (from,to) edge with a positive net,
// owned by DelegationEdgeKey(from,to), stamped at atHeight. Records whose asset
// is already AssetDelegation are ignored so a re-run cannot double-count.
func BackfillDelegationEdges(records []ledgerDb.LedgerRecord, atHeight uint64) []LedgerUpdate {
	type stakePair struct {
		from, to       string
		amount         int64
		hasFrom, hasTo bool
	}
	stakes := map[string]*stakePair{}
	edges := map[string]int64{}

	for _, r := range records {
		if r.Asset == AssetDelegation || r.Asset == AssetDelegationTotal {
			continue // already-migrated / post-activation edge rows — never re-count
		}
		base, suffix := splitLedgerBaseId(r.Id)
		switch r.Type {
		case "consensus_stake":
			p := stakes[base]
			if p == nil {
				p = &stakePair{}
				stakes[base] = p
			}
			switch suffix {
			case "in": // Owner = delegator (from), amount negative
				p.from = r.Owner
				p.hasFrom = true
			case "out": // Owner = node (to), amount positive — authoritative magnitude
				p.to = r.Owner
				p.amount = r.Amount
				p.hasTo = true
			}
		case "consensus_unstake":
			// Legacy unstake: a single "#in" row debits the bond holder's
			// hive_consensus (Owner = node, from == to). Reduce that self-edge.
			if suffix == "in" {
				edges[DelegationEdgeKey(r.Owner, r.Owner)] += r.Amount // r.Amount < 0
			}
		}
	}

	for _, p := range stakes {
		if p.hasFrom && p.hasTo {
			edges[DelegationEdgeKey(p.from, p.to)] += p.amount
		}
	}

	keys := make([]string, 0, len(edges))
	for k := range edges {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic output order

	// Per-node gross total = Σ positive edges to that node (slash-immune), so the
	// pro-rata slash ratio (bond/total) is well-defined for migrated stakes too.
	totals := map[string]int64{}

	out := make([]LedgerUpdate, 0, len(keys))
	for _, k := range keys {
		if edges[k] <= 0 {
			continue // nothing reclaimable on this edge
		}
		_, to := splitDelegationEdgeKey(k)
		totals[to] += edges[k]
		out = append(out, LedgerUpdate{
			Id:          "delegation_backfill:" + k,
			Owner:       k,
			Amount:      edges[k],
			Asset:       AssetDelegation,
			Type:        "consensus_stake",
			BlockHeight: atHeight,
		})
	}

	totalKeys := make([]string, 0, len(totals))
	for n := range totals {
		totalKeys = append(totalKeys, n)
	}
	sort.Strings(totalKeys)
	for _, n := range totalKeys {
		out = append(out, LedgerUpdate{
			Id:          "delegation_backfill_total:" + n,
			Owner:       n,
			Amount:      totals[n],
			Asset:       AssetDelegationTotal,
			Type:        "consensus_stake",
			BlockHeight: atHeight,
		})
	}
	return out
}

// splitDelegationEdgeKey is the inverse of DelegationEdgeKey: it splits a
// composite "from::to" owner back into its two accounts.
func splitDelegationEdgeKey(key string) (from, to string) {
	parts := strings.SplitN(key, delegationEdgeSep, 2)
	if len(parts) != 2 {
		return key, ""
	}
	return parts[0], parts[1]
}

// Delegation-migration marker: a single ledger row written when the one-time
// backfill completes. Its presence means "already migrated" — also true for a
// node restored from a post-activation state snapshot, so the migration is
// correctly skipped there.
const (
	delegationMigrationAccount  = "system:delegation_migration"
	delegationMigrationAsset    = "delegation_migration"
	delegationMigrationMarkerID = "delegation_migration_done"
)

// MigrateDelegationEdgesOnce backfills per-delegator delegation edges from the
// full consensus stake/unstake history, exactly once, at consensus-0.2.0
// activation. The caller MUST gate on delegatedStakeActive(blockHeight) and run
// it BEFORE the slot's transactions execute (so a delegated unstake in the
// activation slot sees its edge). Idempotent: a persisted marker short-circuits
// re-runs, and even without it BackfillDelegationEdges ignores already-seeded
// AssetDelegation/Total rows and StoreLedger upserts by deterministic Id, so a
// crash mid-store re-converges. Deterministic: same canonical ledger -> same
// edges on every node. Returns the number of edges seeded.
func (ls *ledgerSystem) MigrateDelegationEdgesOnce(blockHeight uint64) int {
	if ls.LedgerDb == nil {
		return 0
	}
	// Already migrated? (marker present, or restored from a post-activation snapshot)
	if marker, _ := ls.LedgerDb.GetLedgerRange(
		delegationMigrationAccount, 0, blockHeight, delegationMigrationAsset,
	); marker != nil && len(*marker) > 0 {
		return 0
	}

	records, err := ls.LedgerDb.GetLedgerRecordsByType(
		[]string{"consensus_stake", "consensus_unstake"}, blockHeight)
	if err != nil {
		return 0 // transient read failure — retried next slot (marker still absent)
	}

	edges := BackfillDelegationEdges(records, blockHeight)

	seeded := 0
	out := make([]ledgerDb.LedgerRecord, 0, len(edges)+1)
	for _, e := range edges {
		if e.Asset == AssetDelegation {
			seeded++ // count per-edge rows only (not the per-node total rows)
		}
		out = append(out, ledgerDb.LedgerRecord{
			Id:          e.Id,
			Owner:       e.Owner,
			Amount:      e.Amount,
			Asset:       e.Asset,
			Type:        e.Type,
			BlockHeight: e.BlockHeight,
		})
	}
	// Marker row (Amount = activation height, for audit). Written in the same
	// StoreLedger so it lands with the edges.
	out = append(out, ledgerDb.LedgerRecord{
		Id:          delegationMigrationMarkerID,
		Owner:       delegationMigrationAccount,
		Amount:      int64(blockHeight),
		Asset:       delegationMigrationAsset,
		Type:        delegationMigrationAsset,
		BlockHeight: blockHeight,
	})

	if err := ls.LedgerDb.StoreLedger(out...); err != nil {
		return 0 // store failed — marker absent, retried next slot
	}
	return seeded
}

// splitLedgerBaseId strips a single trailing "#segment" (e.g. "#in", "#out",
// "#edge") from a ledger record id, returning the base id and the segment
// (without the "#"). Ids may carry an idCache ":N" suffix on the base, which is
// preserved on the base side. Returns ("", "") inputs unchanged-ish for ids with
// no "#".
func splitLedgerBaseId(id string) (base, suffix string) {
	i := strings.LastIndex(id, "#")
	if i < 0 {
		return id, ""
	}
	return id[:i], id[i+1:]
}
