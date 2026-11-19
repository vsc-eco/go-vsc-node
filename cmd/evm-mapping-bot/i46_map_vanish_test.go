package main

import (
	"context"
	"math/big"
	"strings"
	"testing"
	"time"
)

// mapCalls counts the map calls submitted to the fake L2.
func mapCalls(t *testing.T, l2 *fakeL2) int {
	t.Helper()
	n := 0
	for _, c := range submittedCalls(t, l2) {
		if c.Action == "map" {
			n++
		}
	}
	return n
}

// I-46 (testnet 11-05 18:38): a map the node no longer knows long after it was
// sent (it lost its nonce to another of the bot's calls and the pool deleted
// it) never failed on chain, so the bot maps the deposit again, as B-R1 re-sends
// a vanished confirmSpend and the BTC bot retries a map that did not land; it
// was dead-lettered for the operator. Up to mapVanishRetries times, then the
// operator; a vanished map that is mapped again and lands clears the count.
func TestI46_AVanishedMapIsMappedAgain(t *testing.T) {
	old := mapResultTimeout
	mapResultTimeout = time.Nanosecond
	t.Cleanup(func() { mapResultTimeout = old })

	l1 := newFakeL1()
	l2, gql, rpc, sub, cfg, vault := bridgeWorld(t, l1, 101)
	l2.status = func(string) string { return "-" } // every map vanishes
	dep := ethDeposit(t, l1.block(101), vault, big.NewInt(1e18), nil)
	cp := &Checkpoint{LastScannedBlock: 100, SentWithdrawals: map[string]SentTx{}, BlockRetries: map[uint64]int{}, PendingMaps: map[string]PendingMap{}}
	tick := func() {
		t.Helper()
		if err := runOnce(context.Background(), rpc, gql, sub, cp, cfg, "vsc1bridge-primary"); err != nil {
			t.Fatal(err)
		}
	}
	h := &botHealth{}
	for i := 1; i <= mapVanishRetries; i++ {
		tick()
		if n := mapCalls(t, l2); n != i {
			t.Fatalf("tick %d: %d map calls, want %d (the vanished map is sent again)", i, n, i)
		}
		if len(cp.DeadLetters) != 1 || !cp.DeadLetters[0].AutoRetry || !strings.EqualFold(cp.DeadLetters[0].DepositTx, dep.Hash().Hex()) {
			t.Fatalf("tick %d: dead letters %+v, want the deposit marked for the bot's own retry", i, cp.DeadLetters)
		}
		h.recordTick(time.Now(), nil, cp)
		if h.deadLetters != 0 {
			t.Fatalf("tick %d: /health counts %d dead letter(s) to map by hand; the bot maps this one itself", i, h.deadLetters)
		}
	}
	tick() // vanishes once more: now left to the operator
	if len(cp.DeadLetters) != 1 || cp.DeadLetters[0].AutoRetry || !strings.Contains(cp.DeadLetters[0].Reason, "vanished 4 times") {
		t.Fatalf("after %d+1 vanishes: dead letters %+v, want one for the operator", mapVanishRetries, cp.DeadLetters)
	}
	calls := mapCalls(t, l2)
	tick()
	if n := mapCalls(t, l2); n != calls {
		t.Fatalf("an operator dead letter was mapped again by the bot (%d -> %d map calls)", calls, n)
	}
	h.recordTick(time.Now(), nil, cp)
	if h.deadLetters != 1 {
		t.Fatalf("/health counts %d dead letters, want the operator's 1", h.deadLetters)
	}
}

// I-46: a vanished map mapped again that lands clears the count; a map that
// FAILED on chain, or is still pending at the timeout, is the operator's as
// before (no automatic retry).
func TestI46_OnlyAVanishedMapIsRetriedAndALandedOneClearsTheCount(t *testing.T) {
	old := mapResultTimeout
	mapResultTimeout = time.Nanosecond
	t.Cleanup(func() { mapResultTimeout = old })

	l1 := newFakeL1()
	l2, gql, rpc, sub, cfg, vault := bridgeWorld(t, l1, 101)
	status := "-"
	l2.status = func(string) string { return status }
	ethDeposit(t, l1.block(101), vault, big.NewInt(1e18), nil)
	cp := &Checkpoint{LastScannedBlock: 100, SentWithdrawals: map[string]SentTx{}, BlockRetries: map[uint64]int{}, PendingMaps: map[string]PendingMap{}}
	tick := func() {
		t.Helper()
		if err := runOnce(context.Background(), rpc, gql, sub, cp, cfg, "vsc1bridge-primary"); err != nil {
			t.Fatal(err)
		}
	}
	tick() // vanishes
	status = "CONFIRMED"
	tick() // mapped again, lands
	if len(cp.DeadLetters) != 0 || len(cp.MapVanishes) != 0 || len(cp.PendingMaps) != 0 {
		t.Fatalf("after the second map landed: dead %+v, vanishes %v, pending %d; want all clear", cp.DeadLetters, cp.MapVanishes, len(cp.PendingMaps))
	}

	for _, st := range []string{"FAILED", "UNCONFIRMED"} {
		l1 := newFakeL1()
		l2, gql, rpc, sub, cfg, vault := bridgeWorld(t, l1, 101)
		l2.status = func(string) string { return st }
		ethDeposit(t, l1.block(101), vault, big.NewInt(1e18), nil)
		cp := &Checkpoint{LastScannedBlock: 100, SentWithdrawals: map[string]SentTx{}, BlockRetries: map[uint64]int{}, PendingMaps: map[string]PendingMap{}}
		for i := 0; i < 2; i++ {
			if err := runOnce(context.Background(), rpc, gql, sub, cp, cfg, "vsc1bridge-primary"); err != nil {
				t.Fatal(err)
			}
		}
		if n := mapCalls(t, l2); n != 1 || len(cp.DeadLetters) != 1 || cp.DeadLetters[0].AutoRetry {
			t.Fatalf("%s: %d map calls, dead letters %+v; want one map and an operator dead letter", st, n, cp.DeadLetters)
		}
	}
}

// I-46: a deposit marked for the bot's own retry that its block no longer
// holds on re-scan goes to the operator instead of being re-scanned every tick.
func TestI46_AnAutoRetryNotFoundOnReScanGoesToTheOperator(t *testing.T) {
	l1 := newFakeL1()
	_, _, rpc, _, cfg, vault := bridgeWorld(t, l1, 101)
	ethDeposit(t, l1.block(101), vault, big.NewInt(1e18), nil)
	other := "0x" + strings.Repeat("ab", 32) // not in block 101
	cp := &Checkpoint{DeadLetters: []DeadLetter{{Block: 101, DepositTx: other, Reason: "vanished", AutoRetry: true}}}
	requeueDeadLetters(rpc, cp, cfg, nil)
	if len(cp.DeadLetters) != 1 || cp.DeadLetters[0].AutoRetry {
		t.Fatalf("dead letters %+v, want it kept for the operator (AutoRetry off)", cp.DeadLetters)
	}
	if len(cp.WaitingDeposits[101]) != 0 {
		t.Fatalf("waiting %v, want nothing re-queued", cp.WaitingDeposits[101])
	}
}
