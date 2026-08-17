package state_engine

import (
	"fmt"
	"time"

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
func (se *StateEngine) ApplyLedgerRemediation(blockHeight uint64) {
	// 0 disables (testnet/devnet, and mainnet until the height is pinned).
	if params.LEDGER_REMEDIATION_HEIGHT == 0 || blockHeight != params.LEDGER_REMEDIATION_HEIGHT {
		return
	}
	if se.LedgerState == nil || se.LedgerState.LedgerDb == nil {
		log.Error("ledger remediation: no ledger db; skipping", "height", blockHeight)
		return
	}

	// Settled state strictly before this height — see the determinism note.
	readHeight := blockHeight - 1

	for _, rem := range params.LEDGER_REMEDIATIONS {
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
		// The one bound that matters is already implicit — we credit the
		// outstanding amount and nothing more. That is what stops a windfall:
		// if the account funded the asset before activation, the negative has
		// self-collected and `bal >= 0` skipped it above; if it partly
		// self-collected, only the remainder is credited. Crediting a fixed
		// table amount instead WOULD hand over spendable value.
		//
		// There is deliberately no ceiling on this figure. Raising a negative
		// to zero never gives the account spendable funds — they can spend
		// exactly 0 afterwards — so a large outstanding amount is not a mint,
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

		creditID := fmt.Sprintf("ledger_remediation_%d#%s#%s", blockHeight, rem.Account, rem.Asset)
		debitID := creditID + "#shortfall"

		// Double-entry: the shortfall account carries the permanent record of
		// value the protocol over-paid, so the write-off never silently
		// inflates supply.
		blockingRemediationWrite(creditID, func() error {
			return se.LedgerState.LedgerDb.StoreLedger(
				ledger_db.LedgerRecord{
					Id:          creditID,
					BlockHeight: blockHeight,
					Amount:      amount,
					Asset:       rem.Asset,
					Owner:       rem.Account,
					Type:        ledgerSystem.LedgerTypeRemediationCredit,
				},
				ledger_db.LedgerRecord{
					Id:          debitID,
					BlockHeight: blockHeight,
					Amount:      -amount,
					Asset:       rem.Asset,
					Owner:       params.LedgerShortfallAccount,
					Type:        ledgerSystem.LedgerTypeRemediationDebit,
				},
			)
		})

		log.Info("ledger remediation: negative balance written off",
			"account", rem.Account, "asset", rem.Asset, "amount", amount,
			"counterparty", params.LedgerShortfallAccount, "height", blockHeight)
	}
}

// blockingRemediationWrite fail-stops the write the same way the rest of the
// deterministic ledger path does (mirrors ledger-system's blockingRetry,
// including its exponential backoff — a bare retry loop would spin the CPU): a
// node that silently skipped this emission would carry a different balance from
// its peers forever, which is precisely the divergence class being cleaned up
// here. Blocking until the DB recovers is the only non-divergent option.
func blockingRemediationWrite(id string, write func() error) {
	const (
		baseDelay = 100 * time.Millisecond
		maxDelay  = 30 * time.Second
	)
	delay := baseDelay
	for attempt := 1; ; attempt++ {
		if err := write(); err == nil {
			if attempt > 1 {
				log.Error("ledger remediation write recovered; resuming", "id", id, "attempts", attempt)
			}
			return
		} else {
			log.Error("ledger remediation write failed; halting until DB recovers (fail-stop)",
				"id", id, "attempt", attempt, "retryIn", delay.String(), "err", err)
		}
		time.Sleep(delay)
		if delay < maxDelay {
			if delay *= 2; delay > maxDelay {
				delay = maxDelay
			}
		}
	}
}
