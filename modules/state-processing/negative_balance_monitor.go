package state_engine

import (
	"strings"

	ledgerDb "vsc-node/modules/db/vsc/ledger"
)

// NegativeBalanceScanInterval is how often (in Hive blocks) the node sweeps for
// accounts holding a negative spendable balance. ~1 hour at 3s blocks: cheap
// enough to run forever, frequent enough that a new negative is noticed the
// same day rather than months later.
const NegativeBalanceScanInterval = 1200

// scanNegativeBalances reports any account whose materialized spendable balance
// is negative.
//
// This exists because NOTHING halts on a negative balance any more. The
// fail-stop guard added during the 2026-08-13 recovery was removed after it
// halted mainnet itself on 2026-08-16 — it fired on legacy, network-consistent
// negatives that every node shared, so an unrelated transfer on one of those
// accounts panicked the whole fleet at the same block.
//
// Removing the halt was right, but it left no signal at all: the ten known
// negatives sat undetected from ~Feb 2026 until they took the chain down. A
// balance that goes negative is still a real accounting defect and the leading
// indicator of the divergence class that caused both halts, so surface it
// loudly and let an operator decide, rather than either halting or ignoring it.
//
// Read-only and side-effect free: it never writes, never panics, and never
// influences block processing, so it cannot itself become a halt vector.
func (se *StateEngine) scanNegativeBalances(blockHeight uint64) {
	if se == nil || se.LedgerState == nil || se.LedgerState.BalanceDb == nil {
		return
	}
	if NegativeBalanceScanInterval == 0 || blockHeight%NegativeBalanceScanInterval != 0 {
		return
	}

	records, err := se.LedgerState.BalanceDb.GetAll(blockHeight)
	if err != nil {
		// Deliberately not fail-stop: this is observability, not consensus. A
		// read failure here must never wedge block processing.
		log.Warn("negative balance scan: could not read balances", "bh", blockHeight, "err", err)
		return
	}

	type hit struct {
		account string
		asset   string
		amount  int64
	}
	hits := make([]hit, 0)
	for _, r := range records {
		// Only real Hive-backed user accounts. system:/protocol accounts carry
		// bookkeeping rows whose negatives are structural, not defects — the
		// shortfall counterparty is deliberately negative, for one.
		if !strings.HasPrefix(r.Account, "hive:") {
			continue
		}
		for _, c := range []struct {
			asset  string
			amount int64
		}{
			{"hbd", r.HBD},
			{"hbd_savings", r.HBD_SAVINGS},
			{"hive", r.Hive},
			{"hive_consensus", r.HIVE_CONSENSUS},
		} {
			if c.amount < 0 {
				hits = append(hits, hit{r.Account, c.asset, c.amount})
			}
		}
	}

	if len(hits) == 0 {
		log.Debug("negative balance scan: clean", "bh", blockHeight, "accounts", len(records))
		return
	}
	for _, h := range hits {
		log.Error("NEGATIVE SPENDABLE BALANCE — accounting defect, not halting",
			"account", h.account, "asset", h.asset, "balance", h.amount, "bh", blockHeight)
	}
	log.Error("negative balance scan: accounts affected", "count", len(hits), "bh", blockHeight)
}

var _ = ledgerDb.BalanceRecord{}

// ScanNegativeBalancesForTest exposes the monitor to the black-box test package.
func (se *StateEngine) ScanNegativeBalancesForTest(blockHeight uint64) {
	se.scanNegativeBalances(blockHeight)
}
