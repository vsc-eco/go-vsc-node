package state_engine_test

import (
	"errors"
	"testing"

	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	ledgerSystem "vsc-node/modules/ledger-system"
	stateEngine "vsc-node/modules/state-processing"

	"github.com/stretchr/testify/assert"
)

const remediationTestHeight = uint64(500)

// newRemediationEnv builds a MAINNET test environment. The remediation is
// network-gated (A6) exactly like CONTRACT_DEPLOYMENT_FEE_START_HEIGHT,
// CONTRACT_UPDATE_HEIGHT and PENDULUM_FEE_FIX_HEIGHT, because
// LEDGER_REMEDIATIONS is a mainnet-specific table of ten mainnet accounts. The
// default mocknet env would therefore skip it entirely.
func newRemediationEnv() *testEnv {
	return newTestEnvWithConsensus(nil, systemconfig.MainnetConfig())
}

// withRemediation pins the activation height and table for one test and
// restores the real mainnet values afterwards, so these tests can never leak
// state into the rest of the package.
func withRemediation(t *testing.T, table []params.LedgerRemediation) {
	t.Helper()
	origHeight, origTable := params.LEDGER_REMEDIATION_HEIGHT, params.LEDGER_REMEDIATIONS
	params.LEDGER_REMEDIATION_HEIGHT = remediationTestHeight
	params.LEDGER_REMEDIATIONS = table
	t.Cleanup(func() {
		params.LEDGER_REMEDIATION_HEIGHT = origHeight
		params.LEDGER_REMEDIATIONS = origTable
	})
}

// seedNegative gives the account a settled negative balance well before the
// activation height, the shape every one of the ten mainnet accounts is in.
func seedNegative(env *testEnv, account, asset string, amount int64) {
	if env.LedgerDb.LedgerRecords == nil {
		env.LedgerDb.LedgerRecords = map[string][]ledgerDb.LedgerRecord{}
	}
	env.LedgerDb.LedgerRecords[account] = append(env.LedgerDb.LedgerRecords[account],
		ledgerDb.LedgerRecord{
			Id:          "legacy_overdebit#" + account,
			BlockHeight: remediationTestHeight - 100,
			Amount:      amount, // negative
			Asset:       asset,
			Owner:       account,
			Type:        "unstake",
		})
}

func remediationRows(env *testEnv, account string) []ledgerDb.LedgerRecord {
	out := make([]ledgerDb.LedgerRecord, 0)
	for _, r := range env.LedgerDb.LedgerRecords[account] {
		if r.Type == ledgerSystem.LedgerTypeRemediationCredit ||
			r.Type == ledgerSystem.LedgerTypeRemediationDebit {
			out = append(out, r)
		}
	}
	return out
}

// The core property: at the activation height the negative is written off to
// exactly zero, double-entry against the keyless shortfall account.
func TestLedgerRemediation_WritesOffNegative_DoubleEntry(t *testing.T) {
	withRemediation(t, []params.LedgerRemediation{
		{Account: "hive:dhedge", Asset: "hbd_savings", Expected: 283},
	})
	env := newRemediationEnv()
	seedNegative(env, "hive:dhedge", "hbd_savings", -283)

	env.SE.ApplyLedgerRemediation(remediationTestHeight)

	credits := remediationRows(env, "hive:dhedge")
	assert.Len(t, credits, 1, "exactly one credit row for the account")
	assert.Equal(t, int64(283), credits[0].Amount, "must credit precisely the outstanding negative")
	assert.Equal(t, ledgerSystem.LedgerTypeRemediationCredit, credits[0].Type)

	debits := remediationRows(env, params.LedgerShortfallAccount)
	assert.Len(t, debits, 1, "the shortfall account must carry the paired entry")
	assert.Equal(t, int64(-283), debits[0].Amount, "double-entry: supply must not be inflated")
	assert.Equal(t, ledgerSystem.LedgerTypeRemediationDebit, debits[0].Type)

	// The whole point: the balance is now zero, not negative.
	assert.Equal(t, int64(0),
		env.SE.LedgerState.GetBalance("hive:dhedge", remediationTestHeight, "hbd_savings"),
		"balance must fold to exactly 0 after the write-off")
}

// ★ THE REINDEX PROPERTY — with a SECOND process, not the same one twice.
//
// The earlier version of this test called ApplyLedgerRemediation twice on the
// SAME StateEngine. The second call returned immediately at
// `if se.ledgerRemediationDone { return }`, so it never re-read the balance and
// never called StoreLedger again: `first == second` and `Len == 1` were
// tautologies and the upsert behaviour it claimed to prove was never exercised.
// (Third instance of the same mistake in this file's history — asserting on the
// shape of the output instead of on the value that matters.)
//
// A reindex or a crash-restart is a NEW process against the SAME database, so
// that is what this models: two StateEngines sharing one MockLedgerDb.
func TestLedgerRemediation_ReplayIsIdempotent(t *testing.T) {
	withRemediation(t, []params.LedgerRemediation{
		{Account: "hive:dhedge", Asset: "hbd_savings", Expected: 283},
	})

	first := newRemediationEnv()
	seedNegative(first, "hive:dhedge", "hbd_savings", -283)
	first.SE.ApplyLedgerRemediation(remediationTestHeight)
	firstRows := remediationRows(first, "hive:dhedge")
	assert.Len(t, firstRows, 1)

	// Second process, same ledger contents (including the row just written).
	second := newRemediationEnv()
	second.LedgerDb.LedgerRecords = first.LedgerDb.LedgerRecords
	second.SE.ApplyLedgerRemediation(remediationTestHeight)

	secondRows := remediationRows(second, "hive:dhedge")
	assert.Equal(t, firstRows, secondRows,
		"a fresh process replaying the activation block must reproduce byte-identical rows")
	assert.Len(t, secondRows, 1,
		"the fixed id must upsert, never append a second credit")
	assert.Equal(t, int64(0),
		second.SE.LedgerState.GetBalance("hive:dhedge", remediationTestHeight, "hbd_savings"),
		"balance stays 0 across the replay — not double-credited, not re-zeroed")

	debits := remediationRows(second, params.LedgerShortfallAccount)
	assert.Len(t, debits, 1, "shortfall side must also stay single")
	assert.Equal(t, int64(-283), debits[0].Amount)
}

// Height gate. Nothing may be emitted BEFORE the activation height, or nodes
// replaying at different speeds would diverge. At or after it (within the
// catch-up window) the emission fires — see LedgerRemediationCatchupBlocks: the
// slot-transition branch is skippable across a restart, so exact equality would
// let a node silently miss the remediation forever.
func TestLedgerRemediation_NeverBeforeActivationHeight(t *testing.T) {
	withRemediation(t, []params.LedgerRemediation{
		{Account: "hive:dhedge", Asset: "hbd_savings", Expected: 283},
	})
	for _, h := range []uint64{remediationTestHeight - 10, 10} {
		env := newRemediationEnv()
		seedNegative(env, "hive:dhedge", "hbd_savings", -283)
		env.SE.ApplyLedgerRemediation(h)
		assert.Empty(t, remediationRows(env, "hive:dhedge"),
			"no remediation may be emitted at height %d (before activation)", h)
	}
}

// ★ RESTART CATCH-UP. A node stopped inside the activation slot resumes with
// slotStatus initialised to the slot it resumes in, so the transition for the
// activation slot never fires. Under exact-equality gating that node would
// silently skip the write-off forever — permanent per-node divergence with
// nothing in the logs. It must still apply when it notices later, and the rows
// must be stamped at the ACTIVATION height, not the slot it noticed in, or the
// catch-up would itself be a divergence.
func TestLedgerRemediation_CatchesUpAfterARestartSkippedTheSlot(t *testing.T) {
	withRemediation(t, []params.LedgerRemediation{
		{Account: "hive:dhedge", Asset: "hbd_savings", Expected: 283},
	})

	onTime := newRemediationEnv()
	seedNegative(onTime, "hive:dhedge", "hbd_savings", -283)
	onTime.SE.ApplyLedgerRemediation(remediationTestHeight)

	// A node that missed the slot entirely and only reaches it 30 slots later.
	late := newRemediationEnv()
	seedNegative(late, "hive:dhedge", "hbd_savings", -283)
	late.SE.ApplyLedgerRemediation(remediationTestHeight + 300)

	lateRows := remediationRows(late, "hive:dhedge")
	assert.Len(t, lateRows, 1, "a node that skipped the activation slot must still apply the write-off")
	assert.Equal(t, remediationRows(onTime, "hive:dhedge"), lateRows,
		"late catch-up must emit records IDENTICAL to the on-time node (same id, height, amount)")
	assert.Equal(t, remediationTestHeight, lateRows[0].BlockHeight,
		"rows must be stamped at the activation height, never the slot they were noticed in")
}

// A node upgraded LONG after the activation height must still apply the
// write-off. There is deliberately no upper bound: one would recreate the very
// hazard the lower bound was relaxed to fix, leaving a late-upgraded node with
// the negatives forever (recoverable only by a full reindex) and a permanent
// cross-node balance disagreement.
func TestLedgerRemediation_AppliesEvenVeryLate(t *testing.T) {
	withRemediation(t, []params.LedgerRemediation{
		{Account: "hive:dhedge", Asset: "hbd_savings", Expected: 283},
	})

	onTime := newRemediationEnv()
	seedNegative(onTime, "hive:dhedge", "hbd_savings", -283)
	onTime.SE.ApplyLedgerRemediation(remediationTestHeight)

	// Weeks later.
	late := newRemediationEnv()
	seedNegative(late, "hive:dhedge", "hbd_savings", -283)
	late.SE.ApplyLedgerRemediation(remediationTestHeight + 500_000)

	assert.Equal(t, remediationRows(onTime, "hive:dhedge"), remediationRows(late, "hive:dhedge"),
		"a very late node must emit records identical to an on-time one")
	assert.Equal(t, int64(0),
		late.SE.LedgerState.GetBalance("hive:dhedge", remediationTestHeight, "hbd_savings"),
		"and must reach a zero balance, not keep the negative forever")
}

// A zero activation height disables the whole mechanism (testnet/devnet, and
// mainnet before the height is pinned).
func TestLedgerRemediation_DisabledWhenHeightZero(t *testing.T) {
	origHeight, origTable := params.LEDGER_REMEDIATION_HEIGHT, params.LEDGER_REMEDIATIONS
	params.LEDGER_REMEDIATION_HEIGHT = 0
	params.LEDGER_REMEDIATIONS = []params.LedgerRemediation{
		{Account: "hive:dhedge", Asset: "hbd_savings", Expected: 283},
	}
	t.Cleanup(func() {
		params.LEDGER_REMEDIATION_HEIGHT = origHeight
		params.LEDGER_REMEDIATIONS = origTable
	})

	env := newRemediationEnv()
	seedNegative(env, "hive:dhedge", "hbd_savings", -283)
	env.SE.ApplyLedgerRemediation(0)
	assert.Empty(t, remediationRows(env, "hive:dhedge"), "height 0 must disable the remediation")
}

// ★ NO GIFTING. If the account funded the asset before activation, the negative
// self-collects and there is nothing to write off. Crediting the table's fixed
// Expected here would hand the account free value.
func TestLedgerRemediation_AbsorbedDebt_CreditsNothing(t *testing.T) {
	withRemediation(t, []params.LedgerRemediation{
		{Account: "hive:dhedge", Asset: "hbd_savings", Expected: 283},
	})
	env := newRemediationEnv()
	seedNegative(env, "hive:dhedge", "hbd_savings", -283)
	// A later deposit more than covers the old debt.
	env.LedgerDb.LedgerRecords["hive:dhedge"] = append(env.LedgerDb.LedgerRecords["hive:dhedge"],
		ledgerDb.LedgerRecord{
			Id:          "later_stake#hive:dhedge",
			BlockHeight: remediationTestHeight - 50,
			Amount:      1000,
			Asset:       "hbd_savings",
			Owner:       "hive:dhedge",
			Type:        "stake",
		})

	env.SE.ApplyLedgerRemediation(remediationTestHeight)

	assert.Empty(t, remediationRows(env, "hive:dhedge"),
		"a non-negative balance must not be credited — that would gift value")
	assert.Equal(t, int64(717),
		env.SE.LedgerState.GetBalance("hive:dhedge", remediationTestHeight, "hbd_savings"),
		"the deposit legitimately absorbed the old debt (1000-283)")
}

// Partial absorption: credit only what is still outstanding, not the stale
// table value.
func TestLedgerRemediation_PartiallyAbsorbed_CreditsOnlyRemainder(t *testing.T) {
	withRemediation(t, []params.LedgerRemediation{
		{Account: "hive:dhedge", Asset: "hbd_savings", Expected: 283},
	})
	env := newRemediationEnv()
	seedNegative(env, "hive:dhedge", "hbd_savings", -283)
	env.LedgerDb.LedgerRecords["hive:dhedge"] = append(env.LedgerDb.LedgerRecords["hive:dhedge"],
		ledgerDb.LedgerRecord{
			Id:          "later_stake#hive:dhedge",
			BlockHeight: remediationTestHeight - 50,
			Amount:      200,
			Asset:       "hbd_savings",
			Owner:       "hive:dhedge",
			Type:        "stake",
		})

	env.SE.ApplyLedgerRemediation(remediationTestHeight)

	credits := remediationRows(env, "hive:dhedge")
	assert.Len(t, credits, 1)
	assert.Equal(t, int64(83), credits[0].Amount,
		"only the remaining 83 is outstanding; crediting the stale 283 would gift 200")
	assert.Equal(t, int64(0),
		env.SE.LedgerState.GetBalance("hive:dhedge", remediationTestHeight, "hbd_savings"))
}

// The shortfall debit must be protocol-meta so it never parks a permanent
// negative SPENDABLE balance on a system account — the exact artifact this
// remediation exists to clear.
func TestLedgerRemediation_ShortfallDebitIsProtocolMeta(t *testing.T) {
	assert.True(t, ledgerSystem.IsProtocolMetaLedgerType(ledgerSystem.LedgerTypeRemediationDebit),
		"the shortfall debit must be excluded from spendable folds")
	assert.False(t, ledgerSystem.IsProtocolMetaLedgerType(ledgerSystem.LedgerTypeRemediationCredit),
		"the recipient credit MUST count — correcting the balance is the point")
}

// Every asset the ten mainnet accounts are negative in must be handled.
func TestLedgerRemediation_AllAssets(t *testing.T) {
	withRemediation(t, []params.LedgerRemediation{
		{Account: "hive:a", Asset: "hbd_savings", Expected: 283},
		{Account: "hive:b", Asset: "hive", Expected: 181999},
		{Account: "hive:c", Asset: "hive_consensus", Expected: 15},
	})
	env := newRemediationEnv()
	seedNegative(env, "hive:a", "hbd_savings", -283)
	seedNegative(env, "hive:b", "hive", -181999)
	seedNegative(env, "hive:c", "hive_consensus", -15)

	env.SE.ApplyLedgerRemediation(remediationTestHeight)

	for _, tc := range []struct {
		acct, asset string
		want        int64
	}{
		{"hive:a", "hbd_savings", 283},
		{"hive:b", "hive", 181999},
		{"hive:c", "hive_consensus", 15},
	} {
		rows := remediationRows(env, tc.acct)
		assert.Len(t, rows, 1, "%s must be remediated", tc.acct)
		assert.Equal(t, tc.want, rows[0].Amount, "%s credit", tc.acct)
		assert.Equal(t, int64(0),
			env.SE.LedgerState.GetBalance(tc.acct, remediationTestHeight, tc.asset),
			"%s must fold to 0", tc.acct)
	}
}

// The shipped mainnet table must stay well-formed: real hive: accounts, known
// spendable assets, positive expectations, no duplicates.
func TestLedgerRemediation_MainnetTableIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	total := map[string]int64{}
	for _, rem := range params.LEDGER_REMEDIATIONS {
		key := rem.Account + "|" + rem.Asset
		assert.False(t, seen[key], "duplicate entry for %s", key)
		seen[key] = true

		assert.Contains(t, []string{"hbd_savings", "hive", "hive_consensus"}, rem.Asset,
			"%s: unexpected asset", rem.Account)
		assert.Greater(t, rem.Expected, int64(0), "%s: expectation must be positive", rem.Account)
		assert.Regexp(t, `^hive:`, rem.Account, "remediation targets real hive accounts only")
		total[rem.Asset] += rem.Expected
	}
	assert.Len(t, params.LEDGER_REMEDIATIONS, 10, "all ten affected accounts must be listed")
	// Totals from the on-chain fold on 2026-08-17.
	assert.Equal(t, int64(114847), total["hbd_savings"], "HBD total")
	assert.Equal(t, int64(456999), total["hive"], "HIVE total")
	assert.Equal(t, int64(10015), total["hive_consensus"], "hive_consensus total")
}

// Drift past the reviewed figure is credited IN FULL — the goal is a zero
// balance, and a residual would need a second coordinated height-gated deploy
// to clear. There is deliberately no ceiling: raising a negative to zero never
// gives the account spendable funds, so a large outstanding amount is a larger
// recorded loss on the shortfall account, not a mint.
func TestLedgerRemediation_DriftIsCreditedInFull(t *testing.T) {
	withRemediation(t, []params.LedgerRemediation{
		{Account: "hive:dhedge", Asset: "hbd_savings", Expected: 283},
	})
	env := newRemediationEnv()
	seedNegative(env, "hive:dhedge", "hbd_savings", -10_000)

	env.SE.ApplyLedgerRemediation(remediationTestHeight)

	credits := remediationRows(env, "hive:dhedge")
	assert.Len(t, credits, 1)
	assert.Equal(t, int64(10_000), credits[0].Amount,
		"the live outstanding amount is credited, not the stale table figure")

	debits := remediationRows(env, params.LedgerShortfallAccount)
	assert.Equal(t, int64(-10_000), debits[0].Amount,
		"the shortfall account absorbs the full loss — supply stays conserved")

	assert.Equal(t, int64(0),
		env.SE.LedgerState.GetBalance("hive:dhedge", remediationTestHeight, "hbd_savings"),
		"the balance must reach zero; a residual would need another deploy to clear")
}

// Drifted emissions must replay byte-identically too.
func TestLedgerRemediation_DriftedCreditIsIdempotent(t *testing.T) {
	withRemediation(t, []params.LedgerRemediation{
		{Account: "hive:dhedge", Asset: "hbd_savings", Expected: 283},
	})
	env := newRemediationEnv()
	seedNegative(env, "hive:dhedge", "hbd_savings", -10_000)

	env.SE.ApplyLedgerRemediation(remediationTestHeight)
	first := remediationRows(env, "hive:dhedge")
	env.SE.ApplyLedgerRemediation(remediationTestHeight)
	second := remediationRows(env, "hive:dhedge")

	assert.Equal(t, first, second, "drifted emission must replay byte-identically")
	assert.Len(t, second, 1, "no second credit row")
	assert.Equal(t, int64(0),
		env.SE.LedgerState.GetBalance("hive:dhedge", remediationTestHeight, "hbd_savings"))
}

// ★ SLOT-BOUNDARY GUARD. ApplyLedgerRemediation is driven from
// se.slotStatus.SlotHeight, and CalculateSlotInfo floors every block to
// blockHeight - (blockHeight % SlotLength). It is therefore only ever invoked
// with multiples of SlotLength. A height pinned off a slot boundary would never
// be reached and the remediation would silently never fire — with nothing in
// the logs to say so. Fail here instead, at CI time, the moment the height is
// pinned.
func TestLedgerRemediation_HeightMustBeOnASlotBoundary(t *testing.T) {
	if params.LEDGER_REMEDIATION_HEIGHT == 0 {
		t.Skip("remediation disabled; nothing to validate yet")
	}
	slotLen := stateEngine.CONSENSUS_SPECS.SlotLength
	assert.Zero(t, params.LEDGER_REMEDIATION_HEIGHT%slotLen,
		"LEDGER_REMEDIATION_HEIGHT (%d) must be a multiple of SlotLength (%d) or it will never be reached",
		params.LEDGER_REMEDIATION_HEIGHT, slotLen)
}

// The test height must model production: ApplyLedgerRemediation is driven from
// slotStatus.SlotHeight, which CalculateSlotInfo floors to a multiple of
// SlotLength, so a test height off a slot boundary would exercise a value the
// production caller can never pass.
//
// (This replaces an assertion that checked the file-local constant against
// itself and claimed the invariant was "behavioural" — there is no modulo check
// inside ApplyLedgerRemediation. The real enforcement is
// TestLedgerRemediation_HeightMustBeOnASlotBoundary, which gates the SHIPPED
// constant. The old comment's "or it will never be reached" was also stale:
// true under the original exact-equality gate, but under the current
// `blockHeight < target` gate a misaligned height simply fires at the next slot
// boundary, up to SlotLength-1 blocks late.)
func TestLedgerRemediation_TestHeightModelsAProductionSlot(t *testing.T) {
	slotLen := stateEngine.CONSENSUS_SPECS.SlotLength
	assert.Zero(t, remediationTestHeight%slotLen,
		"the test height must be one the production caller could actually pass")
}

// ★ A3 — THE LATE-APPLICATION DEFECT (PR #244 review, lordbutterfly).
//
// GetBalance is snapshot-anchored: it folds only records ABOVE the newest
// ledger_balances snapshot. A node that upgrades after the activation height
// has already re-snapshotted the account past `target`, so the credit written
// retroactively at `target` is invisible — and stays invisible, because every
// later snapshot is built from the previous one. The node writes the
// byte-identical row and its balance does not move.
//
// No earlier test caught this because none of them ever seeded a BalanceRecord:
// MockBalanceDb returned nil, so every test folded from genesis and took the
// other branch entirely.
func TestLedgerRemediation_LateApply_WithAdvancedSnapshot_StillReachesZero(t *testing.T) {
	withRemediation(t, []params.LedgerRemediation{
		{Account: "hive:dhedge", Asset: "hbd_savings", Expected: 283},
	})
	env := newRemediationEnv()
	seedNegative(env, "hive:dhedge", "hbd_savings", -283)

	// The node ran past the activation height on the old binary and
	// re-snapshotted the account 40 blocks later, carrying the negative.
	env.BalanceDb.BalanceRecords["hive:dhedge"] = []ledgerDb.BalanceRecord{{
		Account:           "hive:dhedge",
		BlockHeight:       remediationTestHeight + 40,
		HBD_SAVINGS:       -283,
		HBD_MODIFY_HEIGHT: remediationTestHeight + 40,
		HBD_CLAIM_HEIGHT:  remediationTestHeight - 100,
	}}

	// Now it upgrades and applies late.
	env.SE.ApplyLedgerRemediation(remediationTestHeight + 300)

	rows := remediationRows(env, "hive:dhedge")
	assert.Len(t, rows, 1, "the credit row must still be written at the activation height")
	assert.Equal(t, remediationTestHeight, rows[0].BlockHeight)

	assert.Equal(t, int64(0),
		env.SE.LedgerState.GetBalance("hive:dhedge", remediationTestHeight+300, "hbd_savings"),
		"a late node must actually reach zero — writing the row is not enough when "+
			"the balance is snapshot-anchored above it")
}

// The on-time path must NOT touch snapshots: it runs immediately before
// UpdateBalances in the same slot transition, so nothing at or above target
// exists yet and the row is already inside that transition's window.
func TestLedgerRemediation_OnTime_LeavesEarlierSnapshotsAlone(t *testing.T) {
	withRemediation(t, []params.LedgerRemediation{
		{Account: "hive:dhedge", Asset: "hbd_savings", Expected: 283},
	})
	env := newRemediationEnv()
	seedNegative(env, "hive:dhedge", "hbd_savings", -283)
	env.BalanceDb.BalanceRecords["hive:dhedge"] = []ledgerDb.BalanceRecord{{
		Account:           "hive:dhedge",
		BlockHeight:       remediationTestHeight - 50, // strictly BELOW target
		HBD_SAVINGS:       0,
		HBD_MODIFY_HEIGHT: remediationTestHeight - 50,
	}}

	env.SE.ApplyLedgerRemediation(remediationTestHeight)

	assert.Len(t, env.BalanceDb.BalanceRecords["hive:dhedge"], 1,
		"an anchor below target is still valid and must be preserved")
}

// A6 — the remediation must not fire off mainnet. LEDGER_REMEDIATIONS is a
// mainnet-specific table of ten mainnet accounts, and both it and the height
// are package globals; every other height constant in the tree
// (CONTRACT_DEPLOYMENT_FEE_START_HEIGHT, CONTRACT_UPDATE_HEIGHT,
// PENDULUM_FEE_FIX_HEIGHT) is guarded by OnMainnet() for the same reason.
func TestLedgerRemediation_DoesNotFireOffMainnet(t *testing.T) {
	withRemediation(t, []params.LedgerRemediation{
		{Account: "hive:dhedge", Asset: "hbd_savings", Expected: 283},
	})
	for _, sc := range []systemconfig.SystemConfig{
		systemconfig.TestnetConfig(),
		systemconfig.DevnetConfig(),
		systemconfig.MocknetConfig(),
	} {
		env := newTestEnvWithConsensus(nil, sc)
		seedNegative(env, "hive:dhedge", "hbd_savings", -283)
		env.SE.ApplyLedgerRemediation(remediationTestHeight)
		assert.Empty(t, remediationRows(env, "hive:dhedge"),
			"the mainnet remediation table must not be applied on a non-mainnet network")
	}
}

// A5.1 — the remediation credits hive_consensus for two of the ten accounts,
// and the only ledger_state.go change in this branch adds
// LedgerTypeRemediationCredit to hiveConsensusLedgerOps. That list is the
// single source of truth shared by GetBalance and GetConsensusBalanceAt, the
// ERROR-AWARE reader behind the bond inclusion seat gate. Nothing in the repo
// ever constructed such a row and called both readers, so a divergence between
// them — the exact drift the list's own comment forbids — would have been
// invisible.
func TestLedgerRemediation_HiveConsensus_BothReadersAgree(t *testing.T) {
	withRemediation(t, []params.LedgerRemediation{
		{Account: "hive:tanzil2024", Asset: "hive_consensus", Expected: 10000},
	})
	env := newRemediationEnv()
	// Seed with the REAL shape: on mainnet tanzil2024's negative comes from a
	// double consensus_unstake (-10000 twice against a single +10000 stake).
	// The row type matters — GetConsensusBalanceAt filters on
	// hiveConsensusLedgerOps, so a type outside that list is counted by
	// GetBalance and ignored by the gate. Every hive_consensus type present on
	// mainnet (consensus_stake, consensus_unstake, safety_slash_consensus) is
	// in the list, which is what makes the two readers agree.
	env.LedgerDb.LedgerRecords["hive:tanzil2024"] = []ledgerDb.LedgerRecord{
		{Id: "stake#1", BlockHeight: remediationTestHeight - 120, Amount: 10000,
			Asset: "hive_consensus", Owner: "hive:tanzil2024", Type: "consensus_stake"},
		{Id: "unstake#1", BlockHeight: remediationTestHeight - 110, Amount: -10000,
			Asset: "hive_consensus", Owner: "hive:tanzil2024", Type: "consensus_unstake"},
		{Id: "unstake#2", BlockHeight: remediationTestHeight - 100, Amount: -10000,
			Asset: "hive_consensus", Owner: "hive:tanzil2024", Type: "consensus_unstake"},
	}

	env.SE.ApplyLedgerRemediation(remediationTestHeight)

	at := remediationTestHeight + 5
	viaGetBalance := env.SE.LedgerState.GetBalance("hive:tanzil2024", at, "hive_consensus")
	viaConsensus, err := env.SE.LedgerState.GetConsensusBalanceAt("hive:tanzil2024", at)

	assert.NoError(t, err)
	assert.Equal(t, int64(0), viaGetBalance, "the write-off must zero the consensus bond")
	assert.Equal(t, viaGetBalance, viaConsensus,
		"GetBalance and GetConsensusBalanceAt MUST agree — the bond inclusion gate "+
			"reads the latter, so drift between them evicts or over-counts a seat")
}

// A5.2 — the fail-stop retry path had zero coverage: MockLedgerDb.StoreLedger
// could never fail, so blockingRemediationWrite's retry branch was never taken.
// A transient failure must be retried until it lands, not skipped.
func TestLedgerRemediation_TransientWriteFailure_IsRetriedNotSkipped(t *testing.T) {
	withRemediation(t, []params.LedgerRemediation{
		{Account: "hive:dhedge", Asset: "hbd_savings", Expected: 283},
	})
	env := newRemediationEnv()
	seedNegative(env, "hive:dhedge", "hbd_savings", -283)
	// Two transient faults, then success.
	env.LedgerDb.StoreErrs = []error{
		errors.New("connection reset by peer"),
		errors.New("no reachable servers"),
	}

	env.SE.ApplyLedgerRemediation(remediationTestHeight)

	rows := remediationRows(env, "hive:dhedge")
	assert.Len(t, rows, 1,
		"a transient write failure must be retried until the credit lands, never dropped")
	assert.Equal(t, int64(283), rows[0].Amount)
	assert.Equal(t, int64(0),
		env.SE.LedgerState.GetBalance("hive:dhedge", remediationTestHeight, "hbd_savings"))
	assert.Empty(t, env.LedgerDb.StoreErrs, "both injected faults must have been consumed by retries")
}
