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
	if se.sconf != nil && !se.sconf.OnMainnet() {
		return
	}
	target := params.LEDGER_REMEDIATION_HEIGHT
	// 0 disables (mainnet until the height is pinned).
	if target == 0 {
		return
	}
	// At or past the activation height — NOT exact equality, and deliberately
	// with no upper bound.
	//
	// The emission runs in the slot-TRANSITION branch, which a restart can skip
	// entirely: a node stopped inside slot R resumes with slotStatus
	// initialised to the slot it resumes in, so the transition for R never
	// fires. Under exact-equality gating that node would silently miss the
	// write-off forever. An upper bound reintroduces the same hazard for any
	// node upgraded after the window closes — it would keep the negatives
	// permanently, recoverable only by a full reindex, which is the cross-node
	// balance disagreement that halted mainnet on 2026-08-13.
	//
	// Applying late is safe precisely because nothing here depends on WHEN it
	// runs: the amount is read at target-1 (immutable history) and every row is
	// stamped at `target` with a fixed id, so a node catching up days later
	// writes rows byte-identical to one that fired on time, and the upsert makes
	// a repeat a no-op.
	if blockHeight < target {
		return
	}
	if se.LedgerState == nil || se.LedgerState.LedgerDb == nil {
		// Do NOT latch the done flag here: this is a transient wiring/startup
		// condition, and latching would disable the write-off for the whole
		// process instead of retrying on the next slot.
		log.Error("ledger remediation: no ledger db; will retry next slot",
			"activationHeight", target, "slot", blockHeight)
		return
	}
	// Bounded work: the records are upserts, so re-emitting is harmless, but
	// there is no reason to rewrite them on every slot. Once per process is
	// enough; a restart re-runs it and lands on the same rows.
	if se.ledgerRemediationDone {
		return
	}
	se.ledgerRemediationDone = true
	if blockHeight > target {
		log.Warn("ledger remediation: applying LATE — this node did not process the activation slot",
			"activationHeight", target, "slot", blockHeight, "blocksLate", blockHeight-target)
	}

	// Everything below is keyed to the FIXED activation height, never to the
	// slot we happened to notice in. A node that catches up late must emit
	// byte-identical rows (same ids, same BlockHeight, same amounts) to one
	// that fired exactly at the height, or the catch-up would itself become a
	// divergence.
	//
	// Settled state strictly before the activation height — see the
	// determinism note.
	readHeight := target - 1

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

		creditID := fmt.Sprintf("ledger_remediation_%d#%s#%s", target, rem.Account, rem.Asset)
		debitID := creditID + "#shortfall"

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

		// ★ LATE PATH: re-derive the affected snapshots from the ledger.
		//
		// GetBalance is snapshot-anchored — recordHeight = balRecord.BlockHeight
		// + 1, folding only ABOVE it — so a credit written at `target` is
		// invisible to any account whose snapshot already advanced past target,
		// and stays invisible because every later snapshot builds on the
		// previous one. A node that upgrades after the height (without
		// reindexing) would otherwise write the byte-identical credit row and
		// keep the negative forever.
		//
		// This is NOT an increment. An earlier version added the credit to each
		// stale snapshot, which is non-idempotent: it needed a durable marker to
		// run once, and then the marker/write ordering became a crash-safety
		// problem in BOTH directions — write-first double-credits on restart,
		// marker-first silently leaves the correction un-applied. Recomputing
		// the field from the authoritative ledger is idempotent by construction,
		// so it can run every slot, survive any crash, and needs no marker at
		// all.
		//
		// Only the ONE remediated asset is rewritten. The other three fields and
		// HBD_AVG / HBD_MODIFY_HEIGHT / HBD_CLAIM_HEIGHT are left untouched:
		// those accumulators are path-dependent and never rebuilt from the
		// ledger, and six of the ten remediated accounts are on
		// hive/hive_consensus while holding a positive hbd_savings position, so
		// discarding their TWAB would shift the interest denominator for every
		// account on the node.
		//
		// A node applying on time skips all of this: it runs immediately before
		// UpdateBalances in the same slot transition, so no snapshot at or above
		// target exists yet.
		if blockHeight > target {
			se.resyncRemediatedSnapshots(rem.Account, rem.Asset, target)
		}

		log.Info("ledger remediation: negative balance written off",
			"account", rem.Account, "asset", rem.Asset, "amount", amount,
			"counterparty", params.LedgerShortfallAccount, "height", target, "appliedAtSlot", blockHeight)
	}
}

// resyncRemediatedSnapshots rewrites this account's snapshots at or above
// `target` so the ONE remediated asset matches the ledger fold at that
// snapshot's height. Idempotent: a second run recomputes the same values and
// writes nothing new, so it is safe on every slot and after any crash.
func (se *StateEngine) resyncRemediatedSnapshots(account, asset string, target uint64) {
	var snaps []ledger_db.BalanceRecord
	blockingRetry("ledger remediation: list snapshots "+account, func() error {
		rows, err := se.LedgerState.BalanceDb.GetBalanceRecordsFrom(account, target)
		if err != nil {
			return err
		}
		snaps = rows
		return nil
	})

	for _, snap := range snaps {
		want := se.foldLedgerAsset(account, asset, snap.BlockHeight)
		if assetField(snap, asset) == want {
			continue // already correct — this is what makes a re-run a no-op
		}
		fixed := snap
		setAssetField(&fixed, asset, want)
		blockingRetry(fmt.Sprintf("ledger remediation: resync %s @%d", account, snap.BlockHeight), func() error {
			return se.LedgerState.BalanceDb.UpdateBalanceRecord(fixed)
		})
		log.Warn("ledger remediation: rewrote stale balance snapshot from the ledger",
			"account", account, "asset", asset, "bh", snap.BlockHeight,
			"was", assetField(snap, asset), "now", want)
	}
}

// foldLedgerAsset sums every non-meta ledger record for (account, asset) at or
// below blockHeight — the same fold GetBalance performs, but anchored at
// genesis so it cannot inherit a stale snapshot.
func (se *StateEngine) foldLedgerAsset(account, asset string, blockHeight uint64) int64 {
	var total int64
	blockingRetry(fmt.Sprintf("ledger remediation: fold %s %s @%d", account, asset, blockHeight), func() error {
		rows, err := se.LedgerState.LedgerDb.GetLedgerRange(account, 0, blockHeight, asset)
		if err != nil {
			return err
		}
		if rows == nil {
			return fmt.Errorf("nil ledger range for %s at %d", account, blockHeight)
		}
		total = 0
		for _, r := range *rows {
			if ledgerSystem.IsProtocolMetaLedgerType(r.Type) {
				continue
			}
			total += r.Amount
		}
		return nil
	})
	return total
}

func assetField(r ledger_db.BalanceRecord, asset string) int64 {
	switch asset {
	case "hbd":
		return r.HBD
	case "hive":
		return r.Hive
	case "hbd_savings":
		return r.HBD_SAVINGS
	case "hive_consensus":
		return r.HIVE_CONSENSUS
	}
	return 0
}

func setAssetField(r *ledger_db.BalanceRecord, asset string, v int64) {
	switch asset {
	case "hbd":
		r.HBD = v
	case "hive":
		r.Hive = v
	case "hbd_savings":
		r.HBD_SAVINGS = v
	case "hive_consensus":
		r.HIVE_CONSENSUS = v
	}
}
