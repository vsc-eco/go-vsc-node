package gateway

import (
	"errors"
	"fmt"
	"testing"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/common/params"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	ledgerSystem "vsc-node/modules/ledger-system"

	"github.com/vsc-eco/hivego"
)

func mkWithdraw(id string, amount int64, blockHeight uint64) ledgerDb.ActionRecord {
	return ledgerDb.ActionRecord{Id: id, Status: "pending", Amount: amount, Asset: "hbd", To: "hive:bob", Type: "withdraw", BlockHeight: blockHeight}
}

func hasAction(as []ledgerDb.ActionRecord, id string) bool {
	for _, a := range as {
		if a.Id == id {
			return true
		}
	}
	return false
}

// TestSelectEligibleActions_AgesWithdrawals: the value-scaled delay lets dust and
// sufficiently-aged large withdrawals through and holds fresh large ones.
func TestSelectEligibleActions_AgesWithdrawals(t *testing.T) {
	const bh = uint64(5000)
	actions := []ledgerDb.ActionRecord{
		mkWithdraw("dust", 999_999, bh),                                  // < tier1 → 0 delay → eligible now
		mkWithdraw("big-fresh", 100_000_000, bh),                         // tier3 → held
		mkWithdraw("big-aged", 100_000_000, bh-outboundDelayTier3Blocks), // aged exactly → eligible
	}
	sel := selectEligibleActions(actions, true, bh)
	if !hasAction(sel, "dust") || !hasAction(sel, "big-aged") || hasAction(sel, "big-fresh") {
		t.Fatalf("unexpected selection: %v", sel)
	}
}

// TestSelectEligibleActions_NoStarvation is the regression test for the review's
// high finding: a wall of under-aged large withdrawals at the FRONT of the queue
// must not starve newer eligible actions behind them.
func TestSelectEligibleActions_NoStarvation(t *testing.T) {
	const bh = uint64(5000)
	var actions []ledgerDb.ActionRecord
	for i := 0; i < 60; i++ { // 60 fresh tier-3 withdrawals → all under-aged
		actions = append(actions, mkWithdraw(fmt.Sprintf("frozen-%03d", i), 100_000_000, bh))
	}
	for i := 0; i < 5; i++ { // eligible stake-netting behind them (never delayed)
		actions = append(actions, ledgerDb.ActionRecord{Id: fmt.Sprintf("stake-%03d", i), Status: "pending", Amount: 10, Asset: "hbd", Type: "stake", BlockHeight: bh})
	}
	sel := selectEligibleActions(actions, true, bh)
	if len(sel) != 5 {
		t.Fatalf("under-aged withdrawals starved eligible actions: got %d eligible, want 5", len(sel))
	}
	for _, a := range sel {
		if a.Type != "stake" {
			t.Fatalf("an under-aged withdrawal leaked into the batch: %s", a.Id)
		}
	}
}

// TestSelectEligibleActions_CapAppliedToEligible: the batch cap bounds the
// ELIGIBLE set, not the raw fetch.
func TestSelectEligibleActions_CapAppliedToEligible(t *testing.T) {
	const bh = uint64(5000)
	var actions []ledgerDb.ActionRecord
	for i := 0; i < ledgerDb.MaxGatewayActionBatch*2; i++ {
		actions = append(actions, mkWithdraw(fmt.Sprintf("w-%05d", i), 999_999, bh)) // dust → all eligible
	}
	if got := len(selectEligibleActions(actions, true, bh)); got != ledgerDb.MaxGatewayActionBatch {
		t.Fatalf("cap not applied to eligible set: got %d want %d", got, ledgerDb.MaxGatewayActionBatch)
	}
}

// TestSelectEligibleActions_DelayInactivePassesAll: below the version floor
// (delayActive=false) selection is byte-identical to pre-0.6.0 (no age filter).
func TestSelectEligibleActions_DelayInactivePassesAll(t *testing.T) {
	const bh = uint64(5000)
	actions := []ledgerDb.ActionRecord{mkWithdraw("big-fresh", 100_000_000, bh)}
	if got := len(selectEligibleActions(actions, false, bh)); got != 1 {
		t.Fatalf("delay-inactive must pass the fresh large withdrawal, got %d", got)
	}
}

// TestOutboundDelayBlocks pins the value-scaled delay curve (brief fix 4):
// monotonic, dust-fast, hours near vault-draining size.
func TestOutboundDelayBlocks(t *testing.T) {
	cases := []struct {
		amount int64
		want   uint64
	}{
		{0, 0},                                  // zero
		{999_999, 0},                            // < 1,000 coin: dust-fast
		{1_000_000, outboundDelayTier1Blocks},   // 1,000 coin
		{9_999_999, outboundDelayTier1Blocks},   // just under 10,000
		{10_000_000, outboundDelayTier2Blocks},  // 10,000 coin
		{99_999_999, outboundDelayTier2Blocks},  // just under 100,000
		{100_000_000, outboundDelayTier3Blocks}, // 100,000 coin: vault-sized
		{5_000_000_000, outboundDelayTier3Blocks},
	}
	for _, c := range cases {
		if got := outboundDelayBlocks(c.amount); got != c.want {
			t.Fatalf("outboundDelayBlocks(%d) = %d, want %d", c.amount, got, c.want)
		}
	}
	// Monotonic non-decreasing across the tiers.
	prev := uint64(0)
	for _, a := range []int64{0, 1_000_000, 10_000_000, 100_000_000} {
		d := outboundDelayBlocks(a)
		if d < prev {
			t.Fatalf("curve not monotonic at %d: %d < %d", a, d, prev)
		}
		prev = d
	}
}

type fakeAcctClient struct{ acct hivego.AccountData }

func (f fakeAcctClient) GetAccount(_ []string) ([]hivego.AccountData, error) {
	return []hivego.AccountData{f.acct}, nil
}

type fakeLiability map[string]int64

func (f fakeLiability) ExpectedLiability(asset string, _ uint64) (int64, bool) {
	v, ok := f[asset]
	return v, ok
}

func newTestMonitor(mode SolvencyMode, acct hivego.AccountData, liab LiabilitySource, trigger HaltTrigger) *SolvencyMonitor {
	return &SolvencyMonitor{
		mode:          mode,
		gapBps:        100, // 1%
		confirmations: 3,
		interval:      100,
		gatewayWallet: "vsc.gateway",
		accountClient: fakeAcctClient{acct: acct},
		liability:     liab,
		trigger:       trigger,
		breach:        make(map[string]int),
		triggered:     make(map[string]bool),
	}
}

// TestSolvencyMonitor_HaltAfterConfirmations: a sustained shortfall past the
// tolerance trips only after `confirmations` consecutive breaches (hysteresis /
// alarm-before-halt), then fires the on-chain halt trigger.
func TestSolvencyMonitor_HaltAfterConfirmations(t *testing.T) {
	// Observed 900 HBD vs expected 1000 HBD → 100 HBD shortfall, tolerance 1% = 10 HBD.
	acct := hivego.AccountData{
		HbdBalance: "900.000 HBD", SavingsHbdBalance: "0.000 HBD",
		Balance: "0.000 HIVE", SavingsBalance: "0.000 HIVE",
	}
	liab := fakeLiability{"hbd": 1_000_000, "hive": 0}
	fired := 0
	m := newTestMonitor(SolvencyHalt, acct, liab, func(_ uint64, _ string) { fired++ })

	m.Check(1)
	m.Check(2)
	if fired != 0 {
		t.Fatalf("halt fired early after %d checks (want 0)", fired)
	}
	m.Check(3) // 3rd consecutive breach == confirmations
	if fired != 1 {
		t.Fatalf("expected halt trigger to fire once at confirmations, got %d", fired)
	}
}

// TestSolvencyMonitor_HysteresisReset: a single solvent cycle resets the breach
// counter so a flap does not accumulate toward a halt.
func TestSolvencyMonitor_HysteresisReset(t *testing.T) {
	liab := fakeLiability{"hbd": 1_000_000, "hive": 0}
	fired := 0
	trigger := func(_ uint64, _ string) { fired++ }

	breachAcct := hivego.AccountData{HbdBalance: "900.000 HBD", SavingsHbdBalance: "0.000 HBD", Balance: "0.000 HIVE", SavingsBalance: "0.000 HIVE"}
	solventAcct := hivego.AccountData{HbdBalance: "1000.000 HBD", SavingsHbdBalance: "0.000 HBD", Balance: "0.000 HIVE", SavingsBalance: "0.000 HIVE"}

	m := newTestMonitor(SolvencyHalt, breachAcct, liab, trigger)
	m.Check(1) // breach 1
	m.Check(2) // breach 2
	// Flip to solvent for one cycle → reset.
	m.accountClient = fakeAcctClient{acct: solventAcct}
	m.Check(3) // solvent → reset counter
	// Back to breach; must take a fresh run of `confirmations`.
	m.accountClient = fakeAcctClient{acct: breachAcct}
	m.Check(4) // breach 1 (fresh)
	m.Check(5) // breach 2
	if fired != 0 {
		t.Fatalf("halt fired after a reset without 3 fresh consecutive breaches (fired=%d)", fired)
	}
	m.Check(6) // breach 3 → fire
	if fired != 1 {
		t.Fatalf("expected one halt after 3 fresh breaches, got %d", fired)
	}
}

// TestSolvencyMonitor_AlarmModeNeverHalts: in alarm mode the trigger is never
// called even on a sustained breach.
func TestSolvencyMonitor_AlarmModeNeverHalts(t *testing.T) {
	acct := hivego.AccountData{HbdBalance: "0.000 HBD", SavingsHbdBalance: "0.000 HBD", Balance: "0.000 HIVE", SavingsBalance: "0.000 HIVE"}
	liab := fakeLiability{"hbd": 1_000_000, "hive": 0}
	fired := 0
	m := newTestMonitor(SolvencyAlarm, acct, liab, func(_ uint64, _ string) { fired++ })
	for i := uint64(1); i <= 10; i++ {
		m.Check(i)
	}
	if fired != 0 {
		t.Fatalf("alarm mode must never halt, but trigger fired %d times", fired)
	}
}

// TestSolvencyMonitor_DegradesWhenLiabilityUnavailable: with no expected figure
// the monitor takes no action (never trips on missing data).
func TestSolvencyMonitor_DegradesWhenLiabilityUnavailable(t *testing.T) {
	acct := hivego.AccountData{HbdBalance: "0.000 HBD", SavingsHbdBalance: "0.000 HBD", Balance: "0.000 HIVE", SavingsBalance: "0.000 HIVE"}
	fired := 0
	// liability returns ok=false for every asset.
	m := newTestMonitor(SolvencyHalt, acct, fakeLiability{}, func(_ uint64, _ string) { fired++ })
	for i := uint64(1); i <= 5; i++ {
		m.Check(i)
	}
	if fired != 0 {
		t.Fatalf("monitor must degrade (no action) when liability is unavailable, fired=%d", fired)
	}
}

// emptyLedgerDb is a MockLedgerDb with no rows — protocol-held HIVE = 0.
func emptyLedgerDb() *test_utils.MockLedgerDb {
	return &test_utils.MockLedgerDb{LedgerRecords: make(map[string][]ledgerDb.LedgerRecord)}
}

// TestLedgerLiabilitySource_SumsBackedBalances: the real liability source sums
// HBD(+savings) and HIVE(+consensus) across every account's balance record.
func TestLedgerLiabilitySource_SumsBackedBalances(t *testing.T) {
	bdb := &test_utils.MockBalanceDb{BalanceRecords: map[string][]ledgerDb.BalanceRecord{
		"a": {{Account: "a", HBD: 1000, HBD_SAVINGS: 500, Hive: 200, HIVE_CONSENSUS: 50}},
		"b": {{Account: "b", HBD: 300, Hive: 0, HIVE_CONSENSUS: 100}},
	}}
	src := newLedgerLiabilitySource(bdb, emptyLedgerDb())

	if hbd, ok := src.ExpectedLiability("hbd", 10); !ok || hbd != 1800 { // (1000+500)+(300+0)
		t.Fatalf("hbd liability = %d, ok=%v; want 1800,true", hbd, ok)
	}
	if hive, ok := src.ExpectedLiability("hive", 10); !ok || hive != 350 { // (200+50)+(0+100)
		t.Fatalf("hive liability = %d, ok=%v; want 350,true", hive, ok)
	}
	if _, ok := src.ExpectedLiability("btc", 10); ok {
		t.Fatal("unknown asset must return ok=false")
	}
}

// TestLedgerLiabilitySource_ErrorDegrades: a GetAll error surfaces as ok=false so
// the monitor degrades rather than trip on missing data.
func TestLedgerLiabilitySource_ErrorDegrades(t *testing.T) {
	bdb := &test_utils.MockBalanceDb{GetAllErr: errors.New("mongo down")}
	src := newLedgerLiabilitySource(bdb, emptyLedgerDb())
	if _, ok := src.ExpectedLiability("hbd", 1); ok {
		t.Fatal("a GetAll error must surface as ok=false")
	}
	if newLedgerLiabilitySource(nil, emptyLedgerDb()) != nil {
		t.Fatal("nil balanceDb must yield a nil source")
	}
}

// TestLedgerLiabilitySource_CountsProtocolHeldHive: the HIVE leg adds the slash
// residual held on the protocol accounts (reserve credits net of payout debits,
// pending-burn credits net of releases), height-bounded, ignoring marker rows and
// unrelated types — value that left the balance snapshot while the physical HIVE
// stayed on the gateway.
func TestLedgerLiabilitySource_CountsProtocolHeldHive(t *testing.T) {
	bdb := &test_utils.MockBalanceDb{BalanceRecords: map[string][]ledgerDb.BalanceRecord{
		"a": {{Account: "a", Hive: 200, HIVE_CONSENSUS: 50}}, // snapshot: 250
	}}
	ldb := &test_utils.MockLedgerDb{LedgerRecords: map[string][]ledgerDb.LedgerRecord{
		params.ProtocolSlashReserveAccount: {
			// landed slash residual + a promoted one
			{Id: "s1#reserve", Amount: 500, Asset: "hive", BlockHeight: 5, Type: ledgerSystem.LedgerTypeSafetySlashReserve},
			{Id: "s2#promoted", Amount: 300, Asset: "hive", BlockHeight: 6, Type: ledgerSystem.LedgerTypeSafetySlashReserve},
			// governance payout drew down 200 (paired recipient credit re-enters the snapshot)
			{Id: "p1#debit", Amount: -200, Asset: "hive", BlockHeight: 7, Type: ledgerSystem.LedgerTypeReservePayoutDebit},
			// beyond the query height — must be excluded
			{Id: "s3#future", Amount: 9999, Asset: "hive", BlockHeight: 100, Type: ledgerSystem.LedgerTypeSafetySlashReserve},
		},
		params.ProtocolSlashPendingBurnAccount: {
			// one live in-flight residual...
			{Id: "s4#pending", Amount: 250, Asset: "hive", BlockHeight: 5, Type: ledgerSystem.LedgerTypeSafetySlashHiveBurnPending},
			// ...and one fully cancelled (credit + release net to 0; marker is 0)
			{Id: "s5#pending", Amount: 400, Asset: "hive", BlockHeight: 5, Type: ledgerSystem.LedgerTypeSafetySlashHiveBurnPending},
			{Id: "s5#pending#cancel_release", Amount: -400, Asset: "hive", BlockHeight: 6, Type: ledgerSystem.LedgerTypeSafetySlashHiveBurnPendingRelease},
			{Id: "s5#pending#cancelled_marker", Amount: 0, Asset: "hive", BlockHeight: 6, Type: ledgerSystem.LedgerTypeSafetySlashHiveBurnPendingCancelled},
		},
	}}
	src := newLedgerLiabilitySource(bdb, ldb)

	// 250 (snapshot) + [500+300-200] (reserve) + [250+400-400] (pending) = 1100
	if hive, ok := src.ExpectedLiability("hive", 10); !ok || hive != 1100 {
		t.Fatalf("hive liability = %d, ok=%v; want 1100,true", hive, ok)
	}
	// The HBD leg never touches the protocol accounts.
	if hbd, ok := src.ExpectedLiability("hbd", 10); !ok || hbd != 0 {
		t.Fatalf("hbd liability = %d, ok=%v; want 0,true", hbd, ok)
	}
}

// TestLedgerLiabilitySource_HiveDegradesWithoutLedgerDb: with no ledger-records
// reader the HIVE leg degrades (ok=false — a partial figure must never be
// compared) while the HBD leg keeps working.
func TestLedgerLiabilitySource_HiveDegradesWithoutLedgerDb(t *testing.T) {
	bdb := &test_utils.MockBalanceDb{BalanceRecords: map[string][]ledgerDb.BalanceRecord{
		"a": {{Account: "a", HBD: 1000, Hive: 200}},
	}}
	src := newLedgerLiabilitySource(bdb, nil)
	if _, ok := src.ExpectedLiability("hive", 10); ok {
		t.Fatal("hive leg must degrade without a ledger-records reader")
	}
	if hbd, ok := src.ExpectedLiability("hbd", 10); !ok || hbd != 1000 {
		t.Fatalf("hbd leg must keep working: got %d, ok=%v", hbd, ok)
	}
}

// TestSolvencyMonitor_HiveShortfallHalts wires the REAL liability source
// end-to-end on the HIVE leg: expected 700 liquid+bonded + 300 slash reserve =
// 1000 HIVE vs observed 900 → a real 100 HIVE drain trips the halt after
// confirmations. Without the reserve term this drain would sit exactly inside
// the slash-created gap and never alarm.
func TestSolvencyMonitor_HiveShortfallHalts(t *testing.T) {
	bdb := &test_utils.MockBalanceDb{BalanceRecords: map[string][]ledgerDb.BalanceRecord{
		"a": {{Account: "a", Hive: 500_000, HIVE_CONSENSUS: 200_000}}, // 700.000 HIVE
	}}
	ldb := &test_utils.MockLedgerDb{LedgerRecords: map[string][]ledgerDb.LedgerRecord{
		params.ProtocolSlashReserveAccount: {
			{Id: "s1#reserve", Amount: 300_000, Asset: "hive", BlockHeight: 1, Type: ledgerSystem.LedgerTypeSafetySlashReserve}, // 300.000 HIVE
		},
	}}
	acct := hivego.AccountData{
		HbdBalance: "0.000 HBD", SavingsHbdBalance: "0.000 HBD",
		Balance: "900.000 HIVE", SavingsBalance: "0.000 HIVE",
	}
	fired := 0
	m := newTestMonitor(SolvencyHalt, acct, newLedgerLiabilitySource(bdb, ldb), func(_ uint64, _ string) { fired++ })
	m.Check(1)
	m.Check(2)
	if fired != 0 {
		t.Fatalf("halt fired early (fired=%d)", fired)
	}
	m.Check(3)
	if fired != 1 {
		t.Fatalf("expected one halt on the hive leg, got %d", fired)
	}

	// Solvent counterpart: observed covers liquid + bonded + reserve exactly.
	acct.Balance = "1000.000 HIVE"
	fired = 0
	m2 := newTestMonitor(SolvencyHalt, acct, newLedgerLiabilitySource(bdb, ldb), func(_ uint64, _ string) { fired++ })
	for i := uint64(1); i <= 5; i++ {
		m2.Check(i)
	}
	if fired != 0 {
		t.Fatalf("solvent hive leg must not halt (fired=%d)", fired)
	}
}

// TestSolvencyMonitor_WithLedgerLiability_Halts wires the REAL ledger liability
// source end-to-end: expected 1000 HBD vs observed 800 → sustained shortfall
// past the tolerance trips the halt after confirmations.
func TestSolvencyMonitor_WithLedgerLiability_Halts(t *testing.T) {
	bdb := &test_utils.MockBalanceDb{BalanceRecords: map[string][]ledgerDb.BalanceRecord{
		"a": {{Account: "a", HBD: 1_000_000}}, // 1000.000 HBD expected liability
	}}
	acct := hivego.AccountData{HbdBalance: "800.000 HBD", SavingsHbdBalance: "0.000 HBD", Balance: "0.000 HIVE", SavingsBalance: "0.000 HIVE"}
	fired := 0
	m := newTestMonitor(SolvencyHalt, acct, newLedgerLiabilitySource(bdb, emptyLedgerDb()), func(_ uint64, _ string) { fired++ })
	m.Check(1)
	m.Check(2)
	m.Check(3)
	if fired != 1 {
		t.Fatalf("expected one halt via the real ledger liability source, got %d", fired)
	}
}
