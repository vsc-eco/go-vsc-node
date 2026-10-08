package state_engine_test

import (
	"bytes"
	"errors"
	"log/slog"
	"testing"
	"time"

	"vsc-node/lib/vsclog"
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
// SAME StateEngine. The second call returned immediately at a process-local
// done flag (since removed with the late path), so it never re-read the balance
// and never called StoreLedger again: `first == second` and `Len == 1` were
// tautologies and the upsert behaviour it claimed to prove was never exercised.
// (Third instance of the same mistake in this file's history — asserting on the
// shape of the output instead of on the value that matters.)
//
// A reindex replays the activation slot in a NEW process against the SAME
// database, so that is what this models: two StateEngines sharing one
// MockLedgerDb.
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

// Lower half of the height gate. Nothing may be emitted BEFORE the activation
// slot: nodes replaying at different speeds would diverge, and the emission is
// meaningful only at the activation slot itself. The after-the-slot half of
// the gate is covered by TestLedgerRemediation_OnlyAppliesInTheActivationSlot.
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

// ★ EXACT-SLOT GATE. The emission runs only in the slot whose start is the
// activation height — the transition that also snapshots that slot, which is
// the only place the credit is guaranteed to be folded into the snapshot. A
// late write is not a repair: stamped at target and inserted after the
// account's snapshot passed target, it can never be folded (GetBalance reads
// only records ABOVE the snapshot), so the node would look fixed while staying
// short. A node that reaches a later slot without the rows has missed the
// write-off and needs a reindex, which replays the activation slot.
func TestLedgerRemediation_OnlyAppliesInTheActivationSlot(t *testing.T) {
	withRemediation(t, []params.LedgerRemediation{
		{Account: "hive:dhedge", Asset: "hbd_savings", Expected: 283},
	})

	for _, h := range []uint64{
		remediationTestHeight + 10,      // next slot, missed the activation transition
		remediationTestHeight + 300,     // a restart that skipped the slot
		remediationTestHeight + 500_000, // upgraded weeks later
	} {
		env := newRemediationEnv()
		seedNegative(env, "hive:dhedge", "hbd_savings", -283)
		env.SE.ApplyLedgerRemediation(h)
		assert.Empty(t, remediationRows(env, "hive:dhedge"),
			"nothing may be emitted at slot %d — only the activation slot writes", h)
		assert.Equal(t, int64(-283),
			env.SE.LedgerState.GetBalance("hive:dhedge", h, "hbd_savings"),
			"the negative is untouched outside the activation slot")
	}
}

// ★ WIRING — the production call site actually passes the activation height.
//
// Every other test here invokes ApplyLedgerRemediation directly, so none of
// them prove the exact-equality gate is ever satisfied by the real driver.
// The production caller is the slot-transition branch of ProcessBlock, which
// runs ApplyLedgerRemediation(slotStatus.SlotHeight) and then
// UpdateBalances(...) for the slot that just closed. This drives the real
// ProcessBlock path: a block inside the activation slot first (slotStatus
// initialises to target), then the first block of the NEXT slot, whose arrival
// closes the activation slot. A reindex replays stored blocks contiguously,
// so it walks through exactly these two states, and the credit must be written
// and folded into the snapshot at target.
func TestLedgerRemediation_AppliedByTheSlotTransition(t *testing.T) {
	withRemediation(t, []params.LedgerRemediation{
		{Account: "hive:dhedge", Asset: "hbd_savings", Expected: 283},
	})
	env := newRemediationEnv()
	seedNegative(env, "hive:dhedge", "hbd_savings", -283)

	// A block inside the activation slot: slotStatus initialises to target.
	env.Reader.LastBlock = remediationTestHeight - 1
	env.Reader.CreateBlock()
	time.Sleep(200 * time.Millisecond)

	// First block of the next slot: its arrival closes the activation slot and
	// runs ApplyLedgerRemediation(target) through the production call site.
	env.Reader.LastBlock = remediationTestHeight + stateEngine.CONSENSUS_SPECS.SlotLength - 1
	env.Reader.CreateBlock()
	time.Sleep(time.Second)

	rows := remediationRows(env, "hive:dhedge")
	assert.Len(t, rows, 1, "the slot transition must apply the write-off")
	assert.Equal(t, remediationTestHeight, rows[0].BlockHeight,
		"stamped at the activation height, not the block that noticed it")
	assert.Equal(t, int64(283), rows[0].Amount)
	assert.Equal(t, int64(0),
		env.SE.LedgerState.GetBalance("hive:dhedge", remediationTestHeight+stateEngine.CONSENSUS_SPECS.SlotLength, "hbd_savings"),
		"and folded into the activation slot's snapshot")
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
// slotStatus.SlotHeight, a multiple of SlotLength, and the emission is gated on
// blockHeight == target. A test height off a slot boundary would exercise a
// value the production caller can never pass, and the real caller's behaviour
// (an exact-slot match) is what these tests must exercise. The shipped
// constant's alignment is enforced by
// TestLedgerRemediation_HeightMustBeOnASlotBoundary.
func TestLedgerRemediation_TestHeightModelsAProductionSlot(t *testing.T) {
	slotLen := stateEngine.CONSENSUS_SPECS.SlotLength
	assert.Zero(t, remediationTestHeight%slotLen,
		"the test height must be one the production caller could actually pass")
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

// ★ An ON-TIME node that later RESTARTS must be completely inert. The restart
// is past the activation slot, so the exact-slot gate stops it before it can
// rewrite rows or touch balance snapshots. The late path this replaces used to
// re-anchor snapshots on every restart (and, at one point, re-applied the
// credit against snapshots that already folded it). The gate removes that whole
// path; this pins the snapshots — in particular HBD_AVG, which is path-
// dependent and never rebuilt from the ledger — as untouched.
func TestLedgerRemediation_RestartAfterOnTimeApply_KeepsSnapshots(t *testing.T) {
	withRemediation(t, []params.LedgerRemediation{
		{Account: "hive:dhedge", Asset: "hbd_savings", Expected: 283},
	})
	env := newRemediationEnv()
	seedNegative(env, "hive:dhedge", "hbd_savings", -283)

	// Applied on time.
	env.SE.ApplyLedgerRemediation(remediationTestHeight)
	assert.Equal(t, int64(0),
		env.SE.LedgerState.GetBalance("hive:dhedge", remediationTestHeight, "hbd_savings"))

	// Normal operation continues and the account is re-snapshotted past target.
	env.BalanceDb.BalanceRecords["hive:dhedge"] = []ledgerDb.BalanceRecord{{
		Account:           "hive:dhedge",
		BlockHeight:       remediationTestHeight + 60,
		HBD_SAVINGS:       0,
		HBD_AVG:           12345,
		HBD_MODIFY_HEIGHT: remediationTestHeight + 60,
	}}

	// The node restarts: a NEW StateEngine over the same data, past the slot.
	restarted := newRemediationEnv()
	restarted.LedgerDb.LedgerRecords = env.LedgerDb.LedgerRecords
	restarted.BalanceDb.BalanceRecords = env.BalanceDb.BalanceRecords
	restarted.SE.ApplyLedgerRemediation(remediationTestHeight + 200)

	snaps := restarted.BalanceDb.BalanceRecords["hive:dhedge"]
	assert.Len(t, snaps, 1,
		"a restart past the activation slot must not delete snapshots")
	assert.Equal(t, int64(12345), snaps[0].HBD_AVG,
		"HBD_AVG must survive — it is path-dependent and never rebuilt from the ledger")
	assert.Equal(t, int64(0), snaps[0].HBD_SAVINGS,
		"the balance must stay 0 — a restart must not re-apply the credit")
	assert.Equal(t, int64(0),
		restarted.SE.LedgerState.GetBalance("hive:dhedge", remediationTestHeight+200, "hbd_savings"),
		"and the folded balance must agree")
}

// captureSELogs routes the state engine's module logger into a buffer for one
// test, so the remediation's verdicts can be asserted on.
func captureSELogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	l := vsclog.Module("se")
	old := l.Logger
	l.Logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() { l.Logger = old })
	return &buf
}

// ★ A restart past the activation slot must be fully silent: the exact-slot
// gate returns before anything is written or logged. This is the false-alarm
// regression from 10-08 (mainnet operators saw "MUST BE REINDEXED" for all ten
// accounts on every restart of a correct node); the gate deletes the late path
// that caused it, so the assertion is stronger than "no alarm" — the function
// does nothing at all and the stored rows are untouched.
func TestLedgerRemediation_RestartAfterOnTimeApply_IsInert(t *testing.T) {
	withRemediation(t, []params.LedgerRemediation{
		{Account: "hive:dhedge", Asset: "hbd_savings", Expected: 283},
	})
	env := newRemediationEnv()
	seedNegative(env, "hive:dhedge", "hbd_savings", -283)
	env.SE.ApplyLedgerRemediation(remediationTestHeight)
	rows := remediationRows(env, "hive:dhedge")

	// Normal operation snapshots the account past target, with the credit in it.
	env.BalanceDb.BalanceRecords["hive:dhedge"] = []ledgerDb.BalanceRecord{{
		Account: "hive:dhedge", BlockHeight: remediationTestHeight + 60,
		HBD_SAVINGS: 0, HBD_MODIFY_HEIGHT: remediationTestHeight + 60,
	}}

	logs := captureSELogs(t)
	restarted := newRemediationEnv()
	restarted.LedgerDb.LedgerRecords = env.LedgerDb.LedgerRecords
	restarted.BalanceDb.BalanceRecords = env.BalanceDb.BalanceRecords
	restarted.SE.ApplyLedgerRemediation(remediationTestHeight + 200_000)

	assert.NotContains(t, logs.String(), "ledger remediation",
		"a restart of a correct node past the slot must not run or log anything")
	assert.Equal(t, rows, remediationRows(restarted, "hive:dhedge"), "the stored rows are left as they are")
}

// The case the exact-slot gate exists for: a node that ran old code through the
// activation slot has a snapshot past target WITHOUT the credit. A late write
// would be stamped at target, below that snapshot's fold floor, so GetBalance
// could never fold it. The gate refuses to write it. It does not claim a
// reindex either: the balance cannot distinguish this from a masked credit or
// an unrelated new negative, so the node is left exactly as found. Setting it
// right needs an operator reindex, which replays the activation slot.
func TestLedgerRemediation_BehindASnapshot_IsLeftForReindex(t *testing.T) {
	withRemediation(t, []params.LedgerRemediation{
		{Account: "hive:dhedge", Asset: "hbd_savings", Expected: 283},
	})
	env := newRemediationEnv()
	seedNegative(env, "hive:dhedge", "hbd_savings", -283)
	// Snapshotted past target WITHOUT the credit: this node ran old code there.
	env.BalanceDb.BalanceRecords["hive:dhedge"] = []ledgerDb.BalanceRecord{{
		Account: "hive:dhedge", BlockHeight: remediationTestHeight + 60,
		HBD_SAVINGS: -283, HBD_MODIFY_HEIGHT: remediationTestHeight + 60,
	}}

	logs := captureSELogs(t)
	env.SE.ApplyLedgerRemediation(remediationTestHeight + 200)

	assert.Empty(t, remediationRows(env, "hive:dhedge"),
		"no invisible row may be written behind a snapshot that already passed target")
	assert.NotContains(t, logs.String(), "ledger remediation",
		"nothing is run or logged outside the activation slot")
	assert.Equal(t, int64(-283),
		env.SE.LedgerState.GetBalance("hive:dhedge", remediationTestHeight+200, "hbd_savings"),
		"the stale balance is left exactly as found")
}

// Skipping the rewrite needs BOTH rows: a crash between the two upserts leaves
// the credit without its shortfall debit, and a re-run of the activation slot
// must complete the pair. Only a process that resumes inside the slot can
// re-run it, which is what this models.
func TestLedgerRemediation_HalfWrittenPair_IsCompletedOnRerun(t *testing.T) {
	withRemediation(t, []params.LedgerRemediation{
		{Account: "hive:dhedge", Asset: "hbd_savings", Expected: 283},
	})
	env := newRemediationEnv()
	seedNegative(env, "hive:dhedge", "hbd_savings", -283)
	env.SE.ApplyLedgerRemediation(remediationTestHeight)
	// Drop the shortfall side, as a crash between the two upserts would.
	env.LedgerDb.LedgerRecords[params.LedgerShortfallAccount] = nil

	restarted := newRemediationEnv()
	restarted.LedgerDb.LedgerRecords = env.LedgerDb.LedgerRecords
	restarted.SE.ApplyLedgerRemediation(remediationTestHeight)

	debits := remediationRows(restarted, params.LedgerShortfallAccount)
	assert.Len(t, debits, 1, "the missing shortfall row must be written")
	assert.Len(t, remediationRows(restarted, "hive:dhedge"), 1, "and the credit must stay single")
}
