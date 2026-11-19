package main

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// BOT-RC-EXHAUST-1 (testnet 11-17 16:18): the bot ran out of RC during a header
// catch-up. Two ordinary deposits' maps, already accepted by the pool, ran with
// no RC left and failed on chain "minimum RC requirement is not met. RCs
// available: 0". The bot gave up on both ("a map that fails on chain is final").
// Nothing was wrong with the deposits: such a map is put back, waits while the
// bot is short of RC, and goes out again when the RC is back. It is never left
// to the operator for that, however often it happens.
func TestBOTRCEXHAUST1_AMapThatFailedForNoRCIsMappedAgainWhenRCIsBack(t *testing.T) {
	l1 := newFakeL1()
	l2, gql, rpc, sub, cfg, vault := bridgeWorld(t, l1, 101)
	bot := sub.did.String()
	failing := true
	l2.status = func(string) string {
		if failing {
			return "FAILED"
		}
		return "CONFIRMED"
	}
	l2.errMsg = func(string) string { return errRcNotMet + ". RCs available: 0" }
	l2.rc = map[string]int64{bot: 81}
	dep := ethDeposit(t, l1.block(101), vault, big.NewInt(1e18), nil)
	cp := &Checkpoint{LastScannedBlock: 100, SentWithdrawals: map[string]SentTx{}, BlockRetries: map[uint64]int{}, PendingMaps: map[string]PendingMap{}}
	tick := func() {
		t.Helper()
		if err := runOnce(context.Background(), rpc, gql, sub, cp, cfg, "vsc1bridge-primary"); err != nil {
			t.Fatal(err)
		}
	}
	tick()
	if len(l2.submitted) != 1 {
		t.Fatalf("%d submissions after the first tick, want the one map", len(l2.submitted))
	}
	waits := func() bool {
		return len(cp.DeadLetters) == 1 && cp.DeadLetters[0].AutoRetry && cp.DeadLetters[0].RcWait && strings.EqualFold(cp.DeadLetters[0].DepositTx, dep.Hash().Hex())
	}
	if !waits() {
		t.Fatalf("dead letters %+v, want the deposit waiting for RC (AutoRetry, RcWait), not given up on", cp.DeadLetters)
	}
	if rep := newBotHealthReport(cp); strings.Contains(rep, "dead-lettered") {
		t.Errorf("health reports a deposit for the operator: %s", rep)
	}
	// While the bot is short of RC nothing is sent, however many ticks pass.
	for i := 0; i < 5; i++ {
		tick()
	}
	if len(l2.submitted) != 1 || !waits() {
		t.Fatalf("while short of RC: %d submissions, dead letters %+v; want 1 and the deposit still waiting", len(l2.submitted), cp.DeadLetters)
	}
	// Just under twice the map limit: still waiting.
	l2.rc[bot] = int64(rcWaitFactor*cfg.RcLimit) - 1
	tick()
	if len(l2.submitted) != 1 {
		t.Fatalf("%d submissions with RC under %dx the map limit, want 1", len(l2.submitted), rcWaitFactor)
	}
	// RC is back: the deposit is mapped again and this time it lands.
	l2.rc[bot] = int64(rcWaitFactor * cfg.RcLimit)
	failing = false
	tick()
	tick()
	if len(l2.submitted) != 2 {
		t.Fatalf("%d submissions after the RC came back, want 2 (the map again)", len(l2.submitted))
	}
	if len(cp.DeadLetters) != 0 || len(cp.PendingMaps) != 0 {
		t.Fatalf("after the second map landed: dead letters %+v, pending %d; want none", cp.DeadLetters, len(cp.PendingMaps))
	}
}

// A map runs with min(RC the bot holds, its rc_limit), so "cost limit exceeded"
// is also what an ordinary map ends with when the bot is nearly out of RC. It
// gets one more try once the bot holds RC. A second such failure is the
// deposit's own cost: it goes to the operator, with the count in the reason.
func TestBOTRCEXHAUST1_ACostLimitFailureGetsOneRetry(t *testing.T) {
	l1 := newFakeL1()
	l2, gql, rpc, sub, cfg, vault := bridgeWorld(t, l1, 101)
	l2.status = func(string) string { return "FAILED" }
	l2.errMsg = func(string) string { return errCostLimit }
	l2.rc = map[string]int64{sub.did.String(): 1_000_000}
	dep := ethDeposit(t, l1.block(101), vault, big.NewInt(1e18), nil)
	cp := &Checkpoint{LastScannedBlock: 100, SentWithdrawals: map[string]SentTx{}, BlockRetries: map[uint64]int{}, PendingMaps: map[string]PendingMap{}}
	for i := 0; i < 4; i++ {
		if err := runOnce(context.Background(), rpc, gql, sub, cp, cfg, "vsc1bridge-primary"); err != nil {
			t.Fatal(err)
		}
	}
	if len(l2.submitted) != 1+mapCostLimitRetries {
		t.Fatalf("%d map submissions, want %d (the map and %d retry)", len(l2.submitted), 1+mapCostLimitRetries, mapCostLimitRetries)
	}
	if len(cp.DeadLetters) != 1 || cp.DeadLetters[0].AutoRetry || !strings.EqualFold(cp.DeadLetters[0].DepositTx, dep.Hash().Hex()) ||
		!strings.Contains(cp.DeadLetters[0].Reason, "ran out of its RC limit 2 times") {
		t.Fatalf("dead letters %+v, want the deposit left to the operator after its second cost-limit failure", cp.DeadLetters)
	}
}

// A map whose dry run uses up the dry-run cap needs more RC than the bot will
// declare for a call whose cost the depositor chose: sized "from the RC the bot
// holds" it went out with up to all of it, and a call that uses up its limit is
// charged the limit. It is not sent, costs nothing, goes to the operator and
// does not hold its block.
func TestBOTRCEXHAUST1_AMapOverTheDryRunCapIsNotSent(t *testing.T) {
	l1 := newFakeL1()
	l2, gql, rpc, sub, cfg, vault := bridgeWorld(t, l1, 101)
	l2.rc = map[string]int64{sub.did.String(): 900_000}
	l2.sim = func(action string) map[string]any {
		if action == "map" {
			return map[string]any{"success": false, "err": "gas_limit_hit", "err_msg": "cost limit exceeded", "rc_used": int64(simulateRcCap)}
		}
		return map[string]any{"success": true, "ret": "", "rc_used": int64(900)}
	}
	dep := ethDeposit(t, l1.block(101), vault, big.NewInt(1e18), nil)
	cp := &Checkpoint{LastScannedBlock: 100, SentWithdrawals: map[string]SentTx{}, BlockRetries: map[uint64]int{}, PendingMaps: map[string]PendingMap{}}
	if err := runOnce(context.Background(), rpc, gql, sub, cp, cfg, "vsc1bridge-primary"); err != nil {
		t.Fatal(err)
	}
	if len(l2.submitted) != 0 {
		t.Fatalf("%d L2 submissions, want none: the map must not be sent", len(l2.submitted))
	}
	if len(cp.DeadLetters) != 1 || cp.DeadLetters[0].AutoRetry || !strings.EqualFold(cp.DeadLetters[0].DepositTx, dep.Hash().Hex()) ||
		!strings.Contains(cp.DeadLetters[0].Reason, "needs more than 100000 RC") {
		t.Fatalf("dead letters %+v, want the deposit left to the operator with the RC reason", cp.DeadLetters)
	}
	if cp.LastScannedBlock < 101 || len(cp.BlockRetries) != 0 {
		t.Fatalf("the block is held: last scanned %d, retries %v", cp.LastScannedBlock, cp.BlockRetries)
	}
	// Other calls keep the old sizing: only a map is depositor-priced.
	l2.sim = func(string) map[string]any {
		return map[string]any{"success": false, "err": "gas_limit_hit", "err_msg": "cost limit exceeded", "rc_used": int64(simulateRcCap)}
	}
	got, err := sub.sizeCallByDryRun(context.Background(), cfg.ContractID, "drainWithdrawalQueue", "{}", cfg.RcLimit)
	if err != nil || got != 900_000 {
		t.Fatalf("drainWithdrawalQueue sized %d, %v; want 900000 (the RC the bot holds), no error", got, err)
	}
	if _, err := sub.sizeCallByDryRun(context.Background(), cfg.ContractID, "map", "{}", cfg.RcLimit); !errors.Is(err, errMapOverDryRunCap) {
		t.Fatalf("map over the cap: %v, want errMapOverDryRunCap", err)
	}
}

// While the bot is short of RC an RC-waiting deposit is not picked up for the
// automatic retry, but an operator's /retry for it goes out: that request is
// explicit.
func TestBOTRCEXHAUST1_TheAutomaticRetryWaitsForRCAnOperatorRetryDoesNot(t *testing.T) {
	vault := "0x00000000000000000000000000000000000000fa"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Method string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "eth_getBlockByNumber":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"0x65","transactions":[{"hash":"` + dlTx + `","from":"0x00000000000000000000000000000000000000b0","to":"` + vault + `","value":"0xde0b6b3a7640000","transactionIndex":"0x0"}]}}`))
		case "eth_getTransactionReceipt":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"status":"0x1"}}`))
		case "eth_getLogs":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":[]}`))
		default:
			w.WriteHeader(502)
		}
	}))
	t.Cleanup(srv.Close)
	rpc := newEthRPC(srv.URL)
	cfg := EVMBotConfig{VaultAddress: vault, Tokens: map[string]string{}}
	cp := &Checkpoint{rcShort: true, DeadLetters: []DeadLetter{{Block: 101, DepositTx: dlTx, Reason: "map failed on chain: " + errRcNotMet, AutoRetry: true, RcWait: true}}}
	r := newDeadLetterRetries("k")
	r.isDeadLetter = cp.isDeadLettered
	requeueDeadLetters(rpc, cp, cfg, r)
	if len(cp.DeadLetters) != 1 || len(cp.WaitingDeposits[101]) != 0 {
		t.Fatalf("short of RC: dead letters %d, waiting %d; want the deposit still waiting for RC", len(cp.DeadLetters), len(cp.WaitingDeposits[101]))
	}
	if accepted, _, notDead, err := r.request([]string{dlTx}); err != nil || len(accepted) != 1 || len(notDead) != 0 {
		t.Fatalf("operator retry of the RC-waiting deposit: accepted %v, not dead %v, err %v; want it queued", accepted, notDead, err)
	}
	requeueDeadLetters(rpc, cp, cfg, r)
	if len(cp.DeadLetters) != 0 || len(cp.WaitingDeposits[101]) != 1 {
		t.Fatalf("after the operator's retry: dead letters %d, waiting %d; want it parked for mapping", len(cp.DeadLetters), len(cp.WaitingDeposits[101]))
	}
	// With RC back the automatic retry picks it up by itself.
	cp = &Checkpoint{DeadLetters: []DeadLetter{{Block: 101, DepositTx: dlTx, AutoRetry: true, RcWait: true}}}
	requeueDeadLetters(rpc, cp, cfg, nil)
	if len(cp.DeadLetters) != 0 || len(cp.WaitingDeposits[101]) != 1 {
		t.Fatalf("RC back: dead letters %d, waiting %d; want it parked for mapping", len(cp.DeadLetters), len(cp.WaitingDeposits[101]))
	}
}
