package state_engine_test

import (
	"testing"

	"vsc-node/modules/common/params"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	ledgerSystem "vsc-node/modules/ledger-system"

	"github.com/stretchr/testify/assert"
)

const remediationTestHeight = uint64(500)

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
	env := newTestEnv()
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

// ★ THE REINDEX PROPERTY. A reindex drops the ledger and replays from L1, and a
// crash can re-process the same block. Applying twice must be identical to
// applying once — otherwise the second pass would see an already-corrected
// balance and either zero the fix out or double-credit it.
func TestLedgerRemediation_ReplayIsIdempotent(t *testing.T) {
	withRemediation(t, []params.LedgerRemediation{
		{Account: "hive:dhedge", Asset: "hbd_savings", Expected: 283},
	})
	env := newTestEnv()
	seedNegative(env, "hive:dhedge", "hbd_savings", -283)

	env.SE.ApplyLedgerRemediation(remediationTestHeight)
	first := remediationRows(env, "hive:dhedge")

	// Re-process the very same block (crash/restart, or a full replay).
	env.SE.ApplyLedgerRemediation(remediationTestHeight)
	second := remediationRows(env, "hive:dhedge")

	assert.Equal(t, first, second,
		"re-processing the activation block must reproduce byte-identical records")
	assert.Len(t, second, 1, "the fixed id must upsert, never append a second credit")
	assert.Equal(t, int64(0),
		env.SE.LedgerState.GetBalance("hive:dhedge", remediationTestHeight, "hbd_savings"),
		"balance stays 0 across replay — not double-credited, not re-zeroed")

	debits := remediationRows(env, params.LedgerShortfallAccount)
	assert.Len(t, debits, 1, "shortfall side must also stay single")
	assert.Equal(t, int64(-283), debits[0].Amount)
}

// Height gate: nothing may be emitted before or after the activation block, or
// nodes replaying at different speeds would diverge.
func TestLedgerRemediation_OnlyAtActivationHeight(t *testing.T) {
	withRemediation(t, []params.LedgerRemediation{
		{Account: "hive:dhedge", Asset: "hbd_savings", Expected: 283},
	})
	for _, h := range []uint64{remediationTestHeight - 1, remediationTestHeight + 1, 1} {
		env := newTestEnv()
		seedNegative(env, "hive:dhedge", "hbd_savings", -283)
		env.SE.ApplyLedgerRemediation(h)
		assert.Empty(t, remediationRows(env, "hive:dhedge"),
			"no remediation may be emitted at height %d", h)
	}
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

	env := newTestEnv()
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
	env := newTestEnv()
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
	env := newTestEnv()
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
	env := newTestEnv()
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
