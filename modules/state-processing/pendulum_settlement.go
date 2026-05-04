package state_engine

import (
	"strings"

	"vsc-node/modules/db/vsc/elections"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	pendulum_oracle "vsc-node/modules/db/vsc/pendulum_oracle"
	pendulumsettlement "vsc-node/modules/incentive-pendulum/settlement"
	ledgerSystem "vsc-node/modules/ledger-system"
)

// RunPendulumSettlementForTest exposes the unexported runPendulumSettlement
// orchestrator to external test packages. Production callers use the
// settlement.Engine boundary callback wired in New().
func (se *StateEngine) RunPendulumSettlementForTest(info pendulumsettlement.BoundaryInfo) error {
	return se.runPendulumSettlement(info)
}

// NewForPendulumSettlementTest builds a minimum-viable StateEngine wired
// only with the dependencies runPendulumSettlement reads. Avoids spinning
// up the full constructor (which requires a wasm runtime, RC system, etc.)
// just to drive an orchestration unit test.
func NewForPendulumSettlementTest(
	ls ledgerSystem.LedgerSystem,
	electionDb elections.Elections,
	balanceDb ledgerDb.Balances,
	pendulumOracleDb pendulum_oracle.PendulumOracleSnapshots,
	broadcaster pendulumsettlement.Broadcaster,
) *StateEngine {
	return &StateEngine{
		LedgerSystem:        ls,
		electionDb:          electionDb,
		balanceDb:           balanceDb,
		pendulumOracleDb:    pendulumOracleDb,
		pendulumBroadcaster: broadcaster,
	}
}

// runPendulumSettlement is the W5 orchestration the boundary detector fires
// on each epoch transition. The settlement engine has already gated this on
// selfID == leader, so this method runs only on the elected node.
//
// Flow (matches W5 plan steps 1-7):
//
//  1. Read R_nodes from pendulum:nodes bucket at bh-1.
//  2. If R_nodes <= 0, skip — no fees to distribute this epoch.
//  3. Load committee at info.BlockHeight via electionDb; bonds via direct
//     BalanceRecord.HIVE_CONSENSUS read (Phase C, bypasses GetBalance trap).
//  4. Aggregate per-witness slash bps from all snapshots in
//     (prev_epoch_start_bh, bh-1] (Phase A + Phase B).
//  5. Apply slashes -> post-slash bonds + slash list.
//  6. Compute per-account distributions; residual goes to largest
//     post-slash stake (already the existing behavior of
//     ComputeNodeDistributions when fed postSlashBonds).
//  7. Build payload, log, broadcast.
//
// Returns nil on every code path so the settlement engine doesn't surface
// orchestration failures as block-tick errors — failures are logged so an
// operator can trace them, but a missing snapshot or RPC blip should not
// halt block processing.
func (se *StateEngine) runPendulumSettlement(info pendulumsettlement.BoundaryInfo) error {
	txID := pendulumsettlement.BuildDeterministicSettlementTxID(
		info.CurrentEpoch,
		info.PreviousEpoch,
		info.Leader,
	)

	bh := info.BlockHeight
	if bh > 0 {
		bh = bh - 1
	}

	r := se.LedgerSystem.PendulumBucketBalance(ledgerSystem.PendulumNodesHBDBucket, bh)
	if r <= 0 {
		log.Debug(
			"pendulum settlement boundary: empty R bucket, skipping broadcast",
			"epoch", info.CurrentEpoch,
			"prev_epoch", info.PreviousEpoch,
			"tx_id", txID,
			"leader", info.Leader,
		)
		return nil
	}

	electionInfo, err := se.electionDb.GetElectionByHeight(info.BlockHeight)
	if err != nil {
		log.Warn("pendulum settlement: current election lookup failed",
			"epoch", info.CurrentEpoch, "tx_id", txID, "err", err)
		return nil
	}

	// Build "hive:account" form for every committee member; this is the form
	// stored in the balance ledger and the form slash payloads use.
	members := make([]string, 0, len(electionInfo.Members))
	for _, m := range electionInfo.Members {
		acct := m.Account
		if !strings.HasPrefix(acct, "hive:") {
			acct = "hive:" + acct
		}
		members = append(members, acct)
	}

	bonds := pendulumsettlement.ReadCommitteeBonds(se.balanceDb, members, bh)
	if len(bonds) == 0 {
		log.Warn("pendulum settlement: committee has zero HIVE_CONSENSUS, skipping",
			"epoch", info.CurrentEpoch, "tx_id", txID)
		return nil
	}

	// Aggregate slash bps over the closed epoch's snapshot range.
	slashBpsByAccount := se.collectClosedEpochSlashes(info.PreviousEpoch, bh)

	postSlashBonds, appliedSlashes := pendulumsettlement.ApplySlashesToBonds(bonds, slashBpsByAccount)
	totalBondPost := int64(0)
	for _, b := range postSlashBonds {
		totalBondPost += b
	}
	if totalBondPost <= 0 {
		log.Warn("pendulum settlement: post-slash committee bond is zero, skipping",
			"epoch", info.CurrentEpoch, "tx_id", txID)
		return nil
	}

	// FinalNodeShare = R when E unset (the simplified W4-era model: every HBD
	// in the nodes bucket is destined for nodes already; LP retention happens
	// at swap time per W3, not at settlement). CalculateSplitPreviewFixed
	// with E inferred from totalBondPost preserves audit-doc parity with the
	// preview log.
	split := pendulumsettlement.CalculateSplitPreviewFixed(r, totalBondPost, 2, 3, 0, 0)
	nodeShare := split.FinalNodeShare
	if nodeShare <= 0 {
		nodeShare = r // bucket dollars must clear; under the post-W3 design pool side stayed in the pool already.
	}

	nodeDists := pendulumsettlement.ComputeNodeDistributions(nodeShare, postSlashBonds)

	distPayload := make([]pendulumsettlement.DistributionEntry, 0, len(nodeDists))
	for _, d := range nodeDists {
		if d.Amount <= 0 {
			continue
		}
		distPayload = append(distPayload, pendulumsettlement.DistributionEntry{
			Account: d.Account,
			HBDAmt:  d.Amount,
		})
	}
	slashPayload := make([]pendulumsettlement.SlashEntry, 0, len(appliedSlashes))
	for _, s := range appliedSlashes {
		slashPayload = append(slashPayload, pendulumsettlement.SlashEntry{
			Account: s.Account,
			Bps:     s.Bps,
		})
	}
	payload := pendulumsettlement.BuildSettlementPayload(
		info.CurrentEpoch,
		info.PreviousEpoch,
		slashPayload,
		distPayload,
	)

	log.Info(
		"pendulum settlement payload built",
		"epoch", info.CurrentEpoch,
		"prev_epoch", info.PreviousEpoch,
		"tx_id", txID,
		"leader", info.Leader,
		"r_hbd", r,
		"t_hive_post_slash", totalBondPost,
		"node_share_hbd", nodeShare,
		"node_dist_count", len(distPayload),
		"slash_count", len(slashPayload),
	)

	if se.pendulumBroadcaster == nil {
		log.Debug("pendulum settlement: broadcaster not configured, skipping emit",
			"epoch", info.CurrentEpoch, "tx_id", txID)
		return nil
	}
	hiveTxID, err := se.pendulumBroadcaster.Broadcast(payload)
	if err != nil {
		log.Warn("pendulum settlement: broadcast failed",
			"epoch", info.CurrentEpoch, "tx_id", txID, "err", err)
		return nil
	}
	log.Info("pendulum settlement broadcast",
		"epoch", info.CurrentEpoch, "deterministic_tx_id", txID, "hive_tx_id", hiveTxID)
	return nil
}

// collectClosedEpochSlashes aggregates per-witness slash bps from every
// snapshot in (prev_epoch_start_bh, currentBh]. The lower bound is the
// previous election's activation block; if it's missing we fall back to 0
// which conservatively includes every snapshot up to the boundary.
//
// Returned map keys use the "hive:account" form so they line up with the
// committee bonds map; snapshots store witnesses in plain "account" form,
// so we normalize on the way in.
func (se *StateEngine) collectClosedEpochSlashes(prevEpoch uint64, currentBh uint64) map[string]int {
	if se.pendulumOracleDb == nil {
		return nil
	}
	var prevEpochStartBh uint64
	if prevElection := se.electionDb.GetElection(prevEpoch); prevElection != nil {
		prevEpochStartBh = prevElection.BlockHeight
	}

	snaps, err := se.pendulumOracleDb.GetSnapshotsInRange(prevEpochStartBh, currentBh)
	if err != nil || len(snaps) == 0 {
		return nil
	}
	rawTotals := pendulumsettlement.AccumulateSlashBps(snaps)
	if len(rawTotals) == 0 {
		return nil
	}
	out := make(map[string]int, len(rawTotals))
	for w, bps := range rawTotals {
		acct := w
		if !strings.HasPrefix(acct, "hive:") {
			acct = "hive:" + acct
		}
		out[acct] = bps
	}
	return out
}
