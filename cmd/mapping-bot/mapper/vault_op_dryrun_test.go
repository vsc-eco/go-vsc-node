package mapper

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestSizeVaultOpByDryRun(t *testing.T) {
	for _, tc := range []struct {
		used int64
		want uint64
	}{
		{3490, dryRunRcFloor},       // anything under half the floor gets the floor
		{60000, 120000},             // twice the measured use
		{9_000_000, vaultOpRcLimit}, // never above the ceiling
		{0, dryRunRcFloor},          // a zero report still gets the floor
	} {
		gql := &mockGraphQL{sim: &SimulatedCall{Success: true, RcUsed: tc.used}}
		bot, did := buildBotForL2Test(t, gql)
		got, err := bot.sizeVaultOpByDryRun(context.Background(), did.String(), "migrateVault", "{}", vaultOpRcLimit)
		if err != nil || got != tc.want {
			t.Errorf("used %d: got %d, %v; want %d", tc.used, got, err, tc.want)
		}
	}
}

// BOT-RC-1: mainnet's operator DID holds ~466,000 RC. With the dry run a migrateVault
// that uses ~10,700 (a one-input legacy tranche, testnet 2026-10-06) must go out;
// before, the 8,000,000 ceiling refused it forever.
func TestVaultOpDryRun_RealisticBalanceIsEnough(t *testing.T) {
	gql := &mockGraphQL{sim: &SimulatedCall{Success: true, RcUsed: 10678}}
	bot, did := buildBotForL2Test(t, gql)
	gql.accountRC = map[string]int64{did.String(): 465785}

	if _, err := bot.callContractL2(context.Background(), []byte(`{}`), "migrateVault"); err != nil {
		t.Fatalf("a realistic operator balance was refused: %v", err)
	}
	if len(gql.submitted) != 1 {
		t.Fatalf("expected one submission, got %d", len(gql.submitted))
	}
}

// BOT-ERR-1: a dry-run refusal is not submitted (no RC spent) and comes back with
// the contract's reason, which is what the rotation driver's branches read.
func TestVaultOpDryRun_RefusalCarriesTheReasonAndIsNotSubmitted(t *testing.T) {
	for _, tc := range []struct {
		msg   string
		check func(error) bool
		name  string
	}{
		{"migration fee exceeds half the tranche value: sweep deferred", isUneconomicResidual, "uneconomic residual -> writeOffDust"},
		{"action must be performed by the contract owner or the vault operator", isNotOwner, "not the owner/operator -> latch"},
		{"no retiring or draining vault to migrate", isNothingToMigrate, "nothing to migrate"},
	} {
		gql := &mockGraphQL{sim: &SimulatedCall{Success: false, Err: "transaction_error", ErrMsg: tc.msg}}
		bot, did := buildBotForL2Test(t, gql)
		gql.accountRC = map[string]int64{did.String(): 465785}

		_, err := bot.callContractL2(context.Background(), []byte(`{}`), "migrateVault")
		if err == nil || !tc.check(err) {
			t.Errorf("%s: the driver cannot recognise %v", tc.name, err)
		}
		if len(gql.submitted) != 0 {
			t.Errorf("%s: a refused op was submitted", tc.name)
		}
	}
}

// An op heavier than the dry-run cap (a large tranche) declares what the DID holds,
// so a realistic operator can still send it; declaring the 8,000,000 ceiling would
// fail the pre-flight on every cycle and the rotation would never move.
func TestVaultOpDryRun_GasCapDeclaresWhatTheBotHolds(t *testing.T) {
	gql := &mockGraphQL{sim: &SimulatedCall{Success: false, Err: "gas_limit_hit"}}
	bot, did := buildBotForL2Test(t, gql)
	gql.accountRC = map[string]int64{did.String(): 465785}
	if _, err := bot.callContractL2(context.Background(), []byte(`{}`), "migrateVault"); err != nil {
		t.Fatalf("a heavy op was refused with 465,785 RC available: %v", err)
	}
	if len(gql.submitted) != 1 {
		t.Fatalf("expected one submission, got %d", len(gql.submitted))
	}

	// Below the cap the op cannot be paid at all: a clear error, nothing submitted.
	gql2 := &mockGraphQL{sim: &SimulatedCall{Success: false, Err: "gas_limit_hit"}}
	bot2, did2 := buildBotForL2Test(t, gql2)
	gql2.accountRC = map[string]int64{did2.String(): 60000}
	if _, err := bot2.callContractL2(context.Background(), []byte(`{}`), "migrateVault"); err == nil || !strings.Contains(err.Error(), "fund the bot") {
		t.Fatalf("want a fund-the-bot error, got %v", err)
	}
	if len(gql2.submitted) != 0 {
		t.Fatalf("submitted %d with too little RC", len(gql2.submitted))
	}

	// RC unreadable: the ceiling, as before (the node enforces its own limit).
	gql3 := &mockGraphQL{sim: &SimulatedCall{Success: false, Err: "gas_limit_hit"}}
	bot3, did3 := buildBotForL2Test(t, gql3)
	got, err := bot3.sizeVaultOpByDryRun(context.Background(), did3.String(), "migrateVault", "{}", vaultOpRcLimit)
	if err != nil || got != vaultOpRcLimit {
		t.Fatalf("unreadable RC: got %d, %v", got, err)
	}
}

// Only the vault ops are dry-run: map / confirmSpend keep their path unchanged.
func TestVaultOpDryRun_NotForOtherActions(t *testing.T) {
	gql := &mockGraphQL{sim: &SimulatedCall{Success: false, Err: "transaction_error", ErrMsg: "would refuse"}}
	bot, _ := buildBotForL2Test(t, gql)
	if _, err := bot.callContractL2(context.Background(), []byte(`{}`), "confirmSpend"); err != nil && strings.Contains(err.Error(), "dry run") {
		t.Fatalf("confirmSpend went through the vault-op dry run: %v", err)
	}
	if gql.simCalls != 0 {
		t.Fatalf("simulate called %d times for a non-vault action", gql.simCalls)
	}
}

// BOT-LATCH-1: the not-owner latch expires, so an operator appointed after the
// bot started is picked up without a restart.
func TestNotOwnerLatchExpires(t *testing.T) {
	const c = "vsc1LatchTest"
	vaultOpMu.Lock()
	delete(notOwner, c)
	vaultOpMu.Unlock()
	if !markNotOwner(c) || !isNotOwnerLatched(c) {
		t.Fatal("first refusal must latch and report")
	}
	if markNotOwner(c) {
		t.Fatal("a second refusal inside the window must not re-report")
	}
	vaultOpMu.Lock()
	notOwner[c] = time.Now().Add(-notOwnerRecheck - time.Second)
	vaultOpMu.Unlock()
	if isNotOwnerLatched(c) {
		t.Fatal("the latch must expire after notOwnerRecheck")
	}
}

// BOT-RETIRE-1 (testnet 2026-10-06): during the purge grace retireVault succeeds
// with no transition. Each no-op went on chain every two minutes (~1,070 RC each),
// so a no-transition dry run must not be submitted; a retire that advances a
// generation still is.
func TestVaultOpDryRun_RetireWithNoTransitionIsNotSubmitted(t *testing.T) {
	gql := &mockGraphQL{sim: &SimulatedCall{Success: true, Ret: "retire: no generation transitions", RcUsed: 1070}}
	bot, did := buildBotForL2Test(t, gql)
	gql.accountRC = map[string]int64{did.String(): 465785}
	if _, err := bot.callContractL2(context.Background(), []byte(`{}`), "retireVault"); err != errRetireNoTransition {
		t.Fatalf("a no-transition retire returned %v, want errRetireNoTransition", err)
	}
	if len(gql.submitted) != 0 {
		t.Fatalf("a no-transition retire was submitted (%d txs)", len(gql.submitted))
	}

	for _, ret := range []string{"retire: inactivated=0", "retire: purged=0"} {
		gql := &mockGraphQL{sim: &SimulatedCall{Success: true, Ret: ret, RcUsed: 1070}}
		bot, did := buildBotForL2Test(t, gql)
		gql.accountRC = map[string]int64{did.String(): 465785}
		if _, err := bot.callContractL2(context.Background(), []byte(`{}`), "retireVault"); err != nil {
			t.Fatalf("%q: a retire that advances a generation was refused: %v", ret, err)
		}
		if len(gql.submitted) != 1 {
			t.Fatalf("%q: expected one submission, got %d", ret, len(gql.submitted))
		}
	}
}
