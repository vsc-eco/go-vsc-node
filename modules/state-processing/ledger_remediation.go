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

		// ★ LATE PATH: re-anchor the balance snapshot below the credit.
		//
		// GetBalance is snapshot-anchored — ledger_state.go sets
		// recordHeight = balRecord.BlockHeight + 1 and folds only ABOVE that.
		// A credit written retroactively at `target` is therefore INVISIBLE to
		// any account whose ledger_balances snapshot has already advanced past
		// target, and it never becomes visible again because every later
		// snapshot is built from the previous one. A node upgrading late would
		// write the byte-identical row and keep the negative forever.
		//
		// (Found in the PR #244 review and reproduced on live mixed-binary
		// devnet nodes. The earlier reasoning — "applying late is safe because
		// the rows are byte-identical" — checked the rows and never checked
		// that the balance is not derived from the rows alone.)
		//
		// Dropping this account's snapshots at or above target re-anchors the
		// fold below the credit. It also self-heals on the next rebuild:
		// UpdateBalances seeds from prevSnapshot and reads ledger records from
		// prevSnapshot.BlockHeight+1, so once the anchor is below target the
		// credit falls inside that window and lands in the rebuilt snapshot.
		//
		// Only on the late path: applying on time runs immediately before
		// UpdateBalances in the same slot transition, so no snapshot at or
		// above target exists yet and the row is already inside that
		// transition's selection window.
		//
		// KNOWN RESIDUAL: HBD_AVG is a path-dependent accumulator kept only in
		// the snapshot and never rebuilt from the ledger, so a late node
		// re-accrues it from the older anchor and can differ from an on-time
		// node. That is invisible while the account sits at 0 (endingAvg < 1
		// excludes it from the interest distribution on both), and only
		// surfaces if it later funds the asset. A node wanting byte-exact TWAB
		// should reindex.
		// ★ Snapshot re-anchor, and the marker that makes it once-only.
		//
		// GetBalance is snapshot-anchored — recordHeight = balRecord.BlockHeight
		// + 1, folding only ABOVE it — so a credit written at `target` is
		// invisible to any account whose snapshot already advanced past target.
		// A late node would otherwise write the byte-identical row and keep the
		// negative forever.
		//
		// The marker is written on BOTH paths, not just the late one. On-time
		// application needs no adjustment (no snapshot at or above target exists
		// yet), but it must still record that this account is settled —-
		// otherwise the first slot transition after ANY later restart finds no
		// marker and re-applies the adjustment to snapshots that already fold
		// the credit, double-crediting the account. For the two hive_consensus
		// entries that diverges ReadCommitteeBonds and the bond-gate CID.
		//
		// Marker BEFORE the adjustment, deliberately. AdjustBalanceRecordsFrom
		// is a non-idempotent $inc, so the two writes cannot both be safe
		// against a crash between them; ordering the marker first makes the
		// failure UNDER-correction (the negative persists, visible to the
		// negative-balance monitor) rather than a silent double credit.
		markerID := creditID + "#reanchored"
		if !se.remediationMarkerExists(rem.Account, target, markerID) {
			blockingRetry("ledger remediation: marker "+markerID, func() error {
				return se.LedgerState.LedgerDb.StoreLedger(ledger_db.LedgerRecord{
					Id:          markerID,
					BlockHeight: target,
					Amount:      0,
					Asset:       rem.Asset,
					Owner:       rem.Account,
					Type:        ledgerSystem.LedgerTypeRemediationReanchor,
				})
			})
			if blockHeight > target {
				// ADJUST, don't delete: dropping the rows would discard HBD_AVG /
				// HBD_MODIFY_HEIGHT / HBD_CLAIM_HEIGHT (path-dependent, never
				// rebuilt from the ledger) and the other three asset fields. Six
				// of the ten remediated accounts are on hive/hive_consensus and
				// can hold a positive hbd_savings position, so that would shift
				// the interest denominator for every account on the node.
				blockingRetry("ledger remediation: re-anchor "+rem.Account, func() error {
					return se.LedgerState.BalanceDb.AdjustBalanceRecordsFrom(
						rem.Account, target, rem.Asset, amount)
				})
				log.Warn("ledger remediation: adjusted stale balance snapshots so the late credit is visible",
					"account", rem.Account, "asset", rem.Asset, "delta", amount,
					"fromHeight", target, "slot", blockHeight)
			}
		}

		log.Info("ledger remediation: negative balance written off",
			"account", rem.Account, "asset", rem.Asset, "amount", amount,
			"counterparty", params.LedgerShortfallAccount, "height", target, "appliedAtSlot", blockHeight)
	}
}

// remediationMarkerExists reports whether this account's late-path re-anchor
// has already been applied. Durable by design: a process-local flag resets on
// restart, and the balance itself cannot be used as the signal (see the call
// site).
func (se *StateEngine) remediationMarkerExists(account string, target uint64, markerID string) bool {
	var found bool
	blockingRetry("ledger remediation: marker check "+markerID, func() error {
		rows, err := se.LedgerState.LedgerDb.GetLedgerRange(account, target, target, "")
		if err != nil {
			return err
		}
		if rows == nil {
			return fmt.Errorf("nil ledger range for %s at %d", account, target)
		}
		found = false
		for _, r := range *rows {
			if r.Id == markerID {
				found = true
				break
			}
		}
		return nil
	})
	return found
}
