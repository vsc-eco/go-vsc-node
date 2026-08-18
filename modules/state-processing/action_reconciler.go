package state_engine

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
