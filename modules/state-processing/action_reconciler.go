package state_engine

import (
	"encoding/json"

	ledgerSystem "vsc-node/modules/ledger-system"
)

// ActionReconcileInterval is how often (in Hive blocks) the node checks for
// bridge actions marked complete whose credit leg never landed. ~1 hour at 3s
// blocks, matching the negative-balance sweep.
const ActionReconcileInterval = 1200

// reconcileCompletedActions reports any stake/unstake action marked "complete"
// that has no matching `<id>#out` credit row.
//
// This is the detector for the IndexActions failure mode — the one that had no
// detector at all. That function is the SOLE writer of the stake/unstake credit
// leg and used to (a) discard its Get error, so a Mongo fault read as "action
// not found", (b) call ExecuteComplete BEFORE writing the credit, and (c) only
// log if the write failed. Any of the three left an action marked done with the
// user's credit silently missing on this node and nowhere else — the exact
// divergence shape behind both 2026-08 halts.
//
// Those defects are fixed, but nothing ever NOTICED them, which is why they
// survived so long. A reconciliation query over mainnet on 2026-08-18 came back
// clean (0 of 439 complete stake/unstake actions were missing their credit), so
// this starts from a known-good state and exists to catch any recurrence — from
// this path or any future one — while it is still one account rather than a
// halt.
//
// Read-only, non-panicking, and never fail-stop, including on its own read
// errors: it must not become the halt vector that the removed balance guard
// was.
func (se *StateEngine) reconcileCompletedActions(blockHeight uint64) {
	if se == nil || se.LedgerState == nil ||
		se.LedgerState.ActionDb == nil || se.LedgerState.LedgerDb == nil {
		return
	}
	if ActionReconcileInterval == 0 || blockHeight%ActionReconcileInterval != 0 {
		return
	}

	// Only the types whose credit leg IndexActions writes. withdraw and
	// consensus_unstake are settled elsewhere and have no `#out` row here.
	for _, actionType := range []string{"stake", "unstake"} {
		actions, err := se.LedgerState.ActionDb.GetActionsRange(
			nil, nil, nil, []string{actionType}, nil, strPtr("complete"), nil, &blockHeight, 0, 500)
		if err != nil {
			log.Warn("action reconcile: could not read actions",
				"type", actionType, "bh", blockHeight, "err", err)
			continue
		}
		for _, a := range actions {
			asset := "hbd_savings"
			if actionType == "unstake" {
				asset = "hbd"
			}
			rows, err := se.LedgerState.LedgerDb.GetLedgerAfterHeight(a.To, 0, asset, nil)
			if err != nil || rows == nil {
				continue
			}
			found := false
			for _, r := range *rows {
				if r.Id == a.Id+"#out" {
					found = true
					break
				}
			}
			if !found {
				log.Error("ACTION COMPLETE BUT CREDIT MISSING — value owed and never written",
					"action", a.Id, "type", actionType, "to", a.To,
					"amount", a.Amount, "asset", asset, "bh", blockHeight)
			}
		}
	}
}

func strPtr(s string) *string { return &s }

// ReconcileCompletedActionsForTest exposes the reconciler to the black-box test
// package.
func (se *StateEngine) ReconcileCompletedActionsForTest(blockHeight uint64) {
	se.reconcileCompletedActions(blockHeight)
}

// StalePendingActionBlocks is how far past its payout window a gateway action
// may sit "pending" before this node reports it. The gateway action tick runs
// every ACTION_INTERVAL = 20 Hive blocks, so 1200 is 60 consecutive ticks —
// far beyond any ordinary batching delay (the per-batch cap is
// MaxGatewayActionBatch = 100, so clearing a backlog this slowly would need
// ~6000 queued actions, which is why the backlog size is logged alongside).
const StalePendingActionBlocks = 1200

// reconcilePendingActions reports gateway actions still "pending" long after
// the gateway should have paid them.
//
// THIS IS THE DETECTOR THE DOUBLE-PAY CLASS NEVER HAD, and it is the half of
// the problem that no amount of fail-stopping fixes: pay-once rests entirely on
// one status flip (ExecuteComplete), and every way of losing that flip fails
// TOWARDS paying again, silently.
//
// Why a stale "pending" is the right signal for both failure directions:
//
//	re-pay    an action whose L1 payout already mined but whose completion
//	          write was lost stays "pending" forever, so GetPendingActions
//	          re-selects it and the gateway pays it AGAIN every tick (~60 s).
//	          That is the 2026-05 mainnet shape: 34 duplicate action groups,
//	          68 extra payouts, action 24f87dab paid 9x.
//	no-pay    an action whose batch never settled (broadcast failure) also
//	          stays "pending". That is the bradleyarrow shape — 6.482 HBD
//	          stuck in action 4806e7e3...:3 — which nobody noticed for
//	          months because nothing looked.
//
// The two are indistinguishable from the action record alone, and deliberately
// so: either one means real value is unsettled and a human must look. What this
// does NOT do is decide anything. It writes no state, selects nothing, and
// never fail-stops — including on its own read errors — because a detector that
// can halt the chain is a worse bug than the one it detects.
//
// Note on what cannot reach here: executeActions appends action.Id to
// executedOps BEFORE its `continue` guards (bad network prefix, unformattable
// amount), so a skipped-but-batched action is still marked complete at
// settlement. Those are a separate silent-loss path (completed, never paid) and
// are invisible to this query by construction; they belong to
// reconcileCompletedActions' family, not this one.
//
// Read-only, non-panicking, never fail-stop.
func (se *StateEngine) reconcilePendingActions(blockHeight uint64) {
	if se == nil || se.LedgerState == nil || se.LedgerState.ActionDb == nil {
		return
	}
	if ActionReconcileInterval == 0 || blockHeight%ActionReconcileInterval != 0 {
		return
	}
	if blockHeight <= StalePendingActionBlocks {
		return
	}
	cutoff := blockHeight - StalePendingActionBlocks

	// Exactly the types the gateway action tick selects (multisig.executeActions
	// actionFilter). consensus_unstake is excluded on purpose: it is paid on an
	// epoch schedule and is legitimately pending for days, and the POA exit
	// halt holds it longer still.
	stale, err := se.LedgerState.ActionDb.GetActionsRange(
		nil, nil, nil, []string{"withdraw", "stake", "unstake"}, nil,
		strPtr("pending"), nil, &cutoff, 0, 200)
	if err != nil {
		log.Warn("pending-action reconcile: could not read actions",
			"bh", blockHeight, "cutoff", cutoff, "err", err)
		return
	}
	if len(stale) == 0 {
		return
	}
	// Backlog size makes the batching explanation checkable from the log line
	// alone: a backlog below MaxGatewayActionBatch cannot be a queueing delay.
	backlog, err := se.LedgerState.ActionDb.GetPendingActions(blockHeight, "withdraw", "stake", "unstake")
	backlogN := -1
	if err == nil {
		backlogN = len(backlog)
	}
	for _, a := range stale {
		age := uint64(0)
		if blockHeight > a.BlockHeight {
			age = blockHeight - a.BlockHeight
		}
		log.Error("GATEWAY ACTION STILL PENDING PAST ITS PAYOUT WINDOW — either re-paid every tick or stranded",
			"action", a.Id, "type", a.Type, "to", a.To, "amount", a.Amount,
			"asset", a.Asset, "createdAt", a.BlockHeight, "ageBlocks", age,
			"bh", blockHeight, "pendingBacklog", backlogN)
	}
}

// ReconcilePendingActionsForTest exposes the pending-action detector to the
// black-box test package.
func (se *StateEngine) ReconcilePendingActionsForTest(blockHeight uint64) {
	se.reconcilePendingActions(blockHeight)
}

// SettleActionHeader decides whether a mined `vsc.actions` header can be turned
// into settlement, and with what.
//
// A (gated by params.GatewaySettlementParityHeight): the caller used to be a
// bare `if err == nil` with no else, so a header this node could not decode was
// skipped SILENTLY -- and because the header is identical on every node, every
// node skipped it, every node left those actions "pending", and the gateway
// re-selected and re-paid all of them on the next action tick, every ~60 s,
// indefinitely. A type change in any field (encoder/decoder skew across a
// rolling upgrade is the realistic cause) was enough.
//
// encoding/json fills the fields it could read before it errors, so a mismatch
// in `cleared_ops` normally leaves `ops` intact. With parity in force the ops
// list is therefore settled anyway: an undecodable `cleared_ops` degrades
// safely, since IndexActions ignores an undecodable bitmap and every unstake
// then takes the full HBD_UNSTAKE_BLOCKS delay rather than the 1-block neutral
// path. Paying late is recoverable; paying twice is not.
//
// Returns the update, whether to settle, and the decode error (which is
// returned even when settling, so the caller can log it -- a silent recovery
// would hide the skew that caused it).
func SettleActionHeader(raw []byte, parity bool) (ledgerSystem.ActionUpdate, bool, error) {
	var actionUpdate ledgerSystem.ActionUpdate
	err := json.Unmarshal(raw, &actionUpdate)
	if err == nil {
		return actionUpdate, true, nil
	}
	if parity && len(actionUpdate.Ops) > 0 {
		return actionUpdate, true, err
	}
	return actionUpdate, false, err
}
