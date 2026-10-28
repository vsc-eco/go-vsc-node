package main

import (
	"context"
	"math/big"
	"strings"
	"testing"
	"time"
)

// DEADLETTER-DUP: phase C5 ran two bots (RP-8: anyone may map); each deposit's
// second map failed on chain "deposit already processed" and the bot that
// lost dead-lettered a deposit that was credited, so /health stayed unhealthy
// ("map them by hand"). A map that failed because the deposit is already
// credited is done; any other failure is dead-lettered with its reason.
func TestDEADLETTERDUP_AlreadyCreditedMapIsNotDeadLettered(t *testing.T) {
	for _, c := range []struct {
		errMsg, wantReason string
	}{
		{"bad_input: deposit already processed", ""},
		{"transaction_error: proof verification failed", "map failed on chain: transaction_error: proof verification failed"},
		{"", "map failed on chain"},
	} {
		l1 := newFakeL1()
		l2, gql, rpc, sub, cfg, vault := bridgeWorld(t, l1, 101)
		l2.status = func(string) string { return "FAILED" }
		l2.errMsg = func(string) string { return c.errMsg }
		dep := ethDeposit(t, l1.block(101), vault, big.NewInt(1e18), nil)
		cp := &Checkpoint{LastScannedBlock: 100, SentWithdrawals: map[string]SentTx{}, BlockRetries: map[uint64]int{}, PendingMaps: map[string]PendingMap{}}
		if err := runOnce(context.Background(), rpc, gql, sub, cp, cfg, "vsc1bridge-primary"); err != nil {
			t.Fatal(err)
		}
		if len(cp.PendingMaps) != 0 {
			t.Errorf("%q: %d maps still pending", c.errMsg, len(cp.PendingMaps))
		}
		switch {
		case c.wantReason == "" && len(cp.DeadLetters) != 0:
			t.Errorf("%q: a credited deposit was dead-lettered: %+v", c.errMsg, cp.DeadLetters)
		case c.wantReason != "" && (len(cp.DeadLetters) != 1 || cp.DeadLetters[0].DepositTx != dep.Hash().Hex() || cp.DeadLetters[0].Reason != c.wantReason):
			t.Errorf("%q: dead letters %+v, want the deposit with reason %q", c.errMsg, cp.DeadLetters, c.wantReason)
		}
		if c.wantReason == "" && strings.Contains(newBotHealthReport(cp), "dead-lettered") {
			t.Errorf("%q: health still reports a dead letter", c.errMsg)
		}
	}
}

// newBotHealthReport is the /health issues for cp's state.
func newBotHealthReport(cp *Checkpoint) string {
	h := newBotHealth(0)
	now := time.Now()
	h.recordTick(now, nil, cp)
	return strings.Join(h.report(now).Issues, "; ")
}
