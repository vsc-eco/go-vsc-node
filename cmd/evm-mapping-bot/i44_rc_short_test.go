package main

import (
	"context"
	"encoding/json"
	"testing"

	"vsc-node/modules/common"
	transactionpool "vsc-node/modules/transaction-pool"
)

// I-44 (testnet 11-05 14:50): "not enough RCS available" (the account holds
// less RC than the limit) is not a payload that needs more RC: the retries keep
// the configured limit instead of asking 4x and 16x more, which can only be
// refused the same way.
func TestI44_NoEscalationWhenTheAccountHoldsTooLittleRC(t *testing.T) {
	l2, sub := poolWorld(t)
	l2.pool.refuse = 3
	if _, err := sub.callWithRetry(context.Background(), "vsc1bridge", "map", json.RawMessage(`{"d":1}`), 3); err == nil {
		t.Fatal("three refusals did not fail the call")
	}
	l2.mu.Lock()
	defer l2.mu.Unlock()
	if len(l2.submitted) != 3 {
		t.Fatalf("%d attempts, want 3", len(l2.submitted))
	}
	for i, b := range l2.submitted {
		var shell transactionpool.VSCTransactionShell
		if err := common.DecodeCbor(b, &shell); err != nil {
			t.Fatal(err)
		}
		if shell.Headers.RcLimit != sub.cfg.RcLimit {
			t.Fatalf("attempt %d declared rc_limit %d, want the configured %d (no escalation on an RC balance refusal)", i+1, shell.Headers.RcLimit, sub.cfg.RcLimit)
		}
	}
}
