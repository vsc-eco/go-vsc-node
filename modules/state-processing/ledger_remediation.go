package state_engine

import (
	"fmt"

	"vsc-node/modules/common/params"
	ledger_db "vsc-node/modules/db/vsc/ledger"
	ledgerSystem "vsc-node/modules/ledger-system"
)

// applyLedgerRemediation writes off, exactly once, the legacy negative
// spendable balances listed in params.LEDGER_REMEDIATIONS.
//
// Ten mainnet accounts were over-debited between ~2025-08 and ~2026-02 by the
// stale-overstated-balance bug in GetBalance (fixed 2026-06-20, commit
// 5e301259): the spend check read a balance that had not yet subtracted a
// landed debit, so a second debit of the same funds was admitted. The
// over-paid value already left the system on L1, so each negative is a
// realized loss rather than a collectible debt. Left in place it also silently
// eats the account's next deposit, and it was what tripped the (now removed)
// fail-stop guard into halting mainnet on 2026-08-16.
//
// ── Why this survives a reindex ──
//
// A reindex drops every collection except hive_blocks and replays from L1, so
// a row inserted straight into Mongo is destroyed. This emission is CODE on the
// deterministic replay path: every replay reaches this height and re-derives
// byte-identical records. Ids are fixed and StoreLedger upserts on id, so
// applying twice is the same as applying once.
//
// ── Determinism ──
//
// The credit is computed from the account's balance at height-1, which is
// fully settled before this height is processed and therefore identical on
// every node. Reading at height-1 (rather than height) also excludes the
// records emitted here by construction, so a node that re-processes this block
// after a crash recomputes the same amount instead of seeing an
// already-corrected 0 and zeroing the fix out.
//
// The credit is exactly the outstanding negative, so every listed balance ends
// at zero. It is never the table's Expected figure: if the account funded that
// asset before activation the negative self-collects, and crediting a fixed
// amount would hand over spendable value. Expected is documentation — logged
// and compared so drift is visible, never used to decide the credit.
//
// ── Why only the activation slot ──
//
// The emission is gated to blockHeight == target: the one slot transition whose
// slot contains the activation height, the transition that runs
// UpdateBalances(target) immediately after this call. Only there is the credit
// guaranteed to be folded into the same snapshot, on every node and on every
// reindex replay.
//
// Nothing is emitted before it, and nothing after it. A late write is only safe
// while the account's snapshot has not yet passed target, and the balance
// cannot tell that apart from the two cases that make the write invisible or
// wrong: a post-snapshot inflow can mask a still-missing credit (the balance
// reads positive while the credit stays permanently unfolded), and a correctly
// remediated account can go negative again for unrelated reasons. GetBalance
// folds only the records ABOVE the account's snapshot, so a row stamped at
// target and inserted after the snapshot passed target is invisible forever —
// the node would look fixed while staying short. Confining the emission to the
// slot where it is guaranteed visible means a node that reaches a later slot
// without the rows has simply missed the write-off and needs a reindex, which
// replays this slot.
func (se *StateEngine) ApplyLedgerRemediation(blockHeight uint64) {
	// A6: network-gate it, matching every height-constant precedent in the
	// tree — CONTRACT_DEPLOYMENT_FEE_START_HEIGHT, CONTRACT_UPDATE_HEIGHT and
	// PENDULUM_FEE_FIX_HEIGHT are all guarded by OnMainnet(). LEDGER_REMEDIATIONS
	// is a MAINNET-specific table of ten mainnet accounts, and both it and the
	// height are package globals that would otherwise be applied on every
	// network. Inert in practice today (testnet is a separate Hive chain whose
	// heights sit in the low millions against a 109.17M target, and the accounts
	// would read non-negative and be skipped anyway), but the deviation is
	// cheap to close and the convention exists for a reason.
	// Fail CLOSED on a nil sconf: an unknown network must not run a
	// mainnet-specific table. (The inverse, `sconf != nil && !OnMainnet()`, is
	// fail-open — it applies the remediation when the network is unknown.)
	if se.sconf == nil || !se.sconf.OnMainnet() {
		return
	}
	target := params.LEDGER_REMEDIATION_HEIGHT
	// 0 disables (mainnet until the height is pinned).
	if target == 0 {
		return
	}
	// The exact-slot gate. ApplyLedgerRemediation is driven from
	// slotStatus.SlotHeight, which is always a multiple of SlotLength, so this
	// only ever matches when the activation height itself is slot-aligned. That
	// alignment is enforced at CI time by
	// TestLedgerRemediation_HeightMustBeOnASlotBoundary: a misaligned pin would
	// make this comparison never true and the write-off would silently never
	// run. (The row is stamped at target and has to land inside the target
	// slot's snapshot, so the alignment is a real requirement, not just this
	// gate's convenience — the raw BalanceRecord fields are read directly by
	// some consumers with no replay fold to catch a row above the snapshot.)
	if blockHeight != target {
		return
	}
	if se.LedgerState == nil || se.LedgerState.LedgerDb == nil {
		// The only run is this transition; a missing ledger db here cannot be
		// retried later (the slot has passed). The engine cannot process
		// anything without a ledger db, so this is a wiring fault, not a
		// transient state to paper over.
		log.Error("ledger remediation: no ledger db; write-off skipped for this activation slot",
			"activationHeight", target, "slot", blockHeight)
		return
	}

	// Settled state strictly before the activation height — see the
	// determinism note.
	readHeight := target - 1

	alreadyApplied, writtenNow := 0, 0
	for _, rem := range params.LEDGER_REMEDIATIONS {
		creditID := fmt.Sprintf("ledger_remediation_%d#%s#%s", target, rem.Account, rem.Asset)
		debitID := creditID + "#shortfall"

		// A re-run of the activation slot (a reindex replay, or a process that
		// died and resumed inside the slot) can reach this again with the rows
		// already stored. Nothing to redo then. Both rows are checked, not just
		// the credit: a process death between the two upserts leaves the credit
		// without its shortfall debit, and the re-run must complete the pair.
		applied := se.remediationRowsStored(creditID, debitID)
		if applied {
			alreadyApplied++
		} else {
			bal := se.LedgerState.GetBalance(rem.Account, readHeight, rem.Asset)
			if bal >= 0 {
				log.Info("ledger remediation: nothing to write off (balance already non-negative)",
					"account", rem.Account, "asset", rem.Asset, "balance", bal,
					"expected", -rem.Expected)
				continue
			}

			// Credit exactly the outstanding negative: the goal is a zero balance,
			// so anything less leaves a residual that would need a second
			// coordinated height-gated deploy to finish.
			//
			// The one bound that matters is already implicit: we credit the
			// outstanding amount and nothing more. That is what stops a windfall:
			// if the account funded the asset before activation, the negative has
			// self-collected and `bal >= 0` skipped it above; if it partly
			// self-collected, only the remainder is credited. Crediting a fixed
			// table amount instead WOULD hand over spendable value.
			//
			// There is deliberately no ceiling on this figure. Raising a negative
			// to zero never gives the account spendable funds (they can spend
			// exactly 0 afterwards), so a large outstanding amount is not a mint,
			// only a larger recorded loss on the (keyless, double-entry) shortfall
			// account. A ceiling would buy no protection and could only prevent the
			// write-off from doing its job. Drift from the reviewed figure is
			// surfaced loudly below instead.
			amount := -bal
			if amount != rem.Expected {
				log.Warn("ledger remediation: outstanding differs from the reviewed expectation; crediting the live amount",
					"account", rem.Account, "asset", rem.Asset,
					"crediting", amount, "expected", rem.Expected,
					"drift", amount-rem.Expected)
			}

			// Double-entry: the shortfall account carries the permanent record of
			// value the protocol over-paid, so the write-off never silently
			// inflates supply.
			blockingRetry("ledger remediation: "+creditID, func() error {
				return se.LedgerState.LedgerDb.StoreLedger(
					ledger_db.LedgerRecord{
						Id:          creditID,
						BlockHeight: target,
						Amount:      amount,
						Asset:       rem.Asset,
						Owner:       rem.Account,
						Type:        ledgerSystem.LedgerTypeRemediationCredit,
					},
					ledger_db.LedgerRecord{
						Id:          debitID,
						BlockHeight: target,
						Amount:      -amount,
						Asset:       rem.Asset,
						Owner:       params.LedgerShortfallAccount,
						Type:        ledgerSystem.LedgerTypeRemediationDebit,
					},
				)
			})
			writtenNow++
			log.Info("ledger remediation: negative balance written off",
				"account", rem.Account, "asset", rem.Asset, "amount", amount,
				"counterparty", params.LedgerShortfallAccount, "height", target, "appliedAtSlot", blockHeight)
		}
	}
	log.Info("ledger remediation: checked",
		"accounts", len(params.LEDGER_REMEDIATIONS), "alreadyApplied", alreadyApplied, "writtenNow", writtenNow,
		"activationHeight", target, "slot", blockHeight)
}

// remediationRowsStored reports whether both rows of one write-off are stored.
// A read error answers false, which takes the normal path: rewriting the rows is
// an idempotent upsert.
func (se *StateEngine) remediationRowsStored(creditID, debitID string) bool {
	recs, err := se.LedgerState.LedgerDb.GetLedgersByTxId(creditID)
	if err != nil {
		return false
	}
	credit, debit := false, false
	for _, r := range recs {
		switch r.Id {
		case creditID:
			credit = true
		case debitID:
			debit = true
		}
	}
	return credit && debit
}
