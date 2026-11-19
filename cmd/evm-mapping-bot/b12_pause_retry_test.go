package main

import (
	"context"
	"math/big"
	"strings"
	"testing"
	"time"
)

// B-12: a map that ran while the bridge was paused (sent just before the pause
// landed) failed on chain with "contract is paused" and was dead-lettered for
// the operator, though nothing is wrong with the deposit. Maps wait while the
// bridge is paused (I-13), so it is put on the bounded auto-retry path a
// vanished map takes: parked during the pause, mapped again after it. Any other
// on-chain failure is still left to the operator.
func TestB12_MapFailedOnAPauseIsRetried(t *testing.T) {
	for _, c := range []struct {
		errMsg    string
		autoRetry bool
	}{
		{"init_error: contract is paused", true},
		{"proof_failed: merkle proof verification failed", false},
	} {
		l1 := newFakeL1()
		l2, gql, rpc, sub, cfg, vault := bridgeWorld(t, l1, 101)
		l2.status = func(string) string { return "FAILED" }
		msg := c.errMsg
		l2.errMsg = func(string) string { return msg }
		dep := ethDeposit(t, l1.block(101), vault, big.NewInt(1e18), nil)
		cp := &Checkpoint{LastScannedBlock: 100, SentWithdrawals: map[string]SentTx{}, BlockRetries: map[uint64]int{}, PendingMaps: map[string]PendingMap{}}
		if err := runOnce(context.Background(), rpc, gql, sub, cp, cfg, "vsc1bridge-primary"); err != nil {
			t.Fatal(err)
		}
		if len(cp.DeadLetters) != 1 || cp.DeadLetters[0].DepositTx != dep.Hash().Hex() || !strings.Contains(cp.DeadLetters[0].Reason, msg) {
			t.Fatalf("%q: dead letters %+v, want the deposit with its reason", msg, cp.DeadLetters)
		}
		if cp.DeadLetters[0].AutoRetry != c.autoRetry {
			t.Fatalf("%q: AutoRetry %v, want %v", msg, cp.DeadLetters[0].AutoRetry, c.autoRetry)
		}
	}
}

// End to end: the pause-failed deposit waits while the bridge stays paused and
// is mapped again, by the bot, once it is unpaused.
func TestB12_PauseFailedDepositIsMappedAfterTheUnpause(t *testing.T) {
	l1 := newFakeL1()
	l2, gql, rpc, sub, cfg, vault := bridgeWorld(t, l1, 101)
	paused := true
	l2.status = func(string) string {
		if paused {
			return "FAILED"
		}
		return "CONFIRMED"
	}
	l2.errMsg = func(string) string {
		if paused {
			return "init_error: contract is paused"
		}
		return ""
	}
	dep := ethDeposit(t, l1.block(101), vault, big.NewInt(1e18), nil)
	cp := &Checkpoint{LastScannedBlock: 100, SentWithdrawals: map[string]SentTx{}, BlockRetries: map[uint64]int{}, PendingMaps: map[string]PendingMap{}}
	tick := func() {
		t.Helper()
		if err := runOnce(context.Background(), rpc, gql, sub, cp, cfg, "vsc1bridge-primary"); err != nil {
			t.Fatal(err)
		}
	}
	tick() // the map runs into the pause that just landed
	if n := mapCalls(t, l2); n != 1 {
		t.Fatalf("%d map calls, want 1", n)
	}
	cfg.Paused = true // the loop's pause read (main.go I-13) sees the pause
	tick()            // still paused: parked, not sent
	if n := mapCalls(t, l2); n != 1 {
		t.Fatalf("paused tick: %d map calls, want still 1 (maps wait while paused)", n)
	}
	cfg.Paused = false
	paused = false
	tick() // unpaused: mapped again by the bot
	if n := mapCalls(t, l2); n != 2 {
		t.Fatalf("after the unpause: %d map calls, want 2 (the deposit mapped again)", n)
	}
	tick() // the retry's result is read: confirmed
	h := &botHealth{}
	h.recordTick(time.Now(), nil, cp)
	for _, d := range cp.DeadLetters {
		if strings.EqualFold(d.DepositTx, dep.Hash().Hex()) && !d.AutoRetry {
			t.Fatalf("the deposit is left to the operator: %+v", d)
		}
	}
	if h.deadLetters != 0 || len(cp.PendingMaps) != 0 {
		t.Fatalf("/health counts %d manual dead letters, %d maps pending; want 0 and 0", h.deadLetters, len(cp.PendingMaps))
	}
}
