package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// LOCALONLY-EMPTY-1, bot side: a node that could not read the contract's state
// says so instead of dry-running on empty state. For a vault op that is "wait a
// tick", not "no dry run: send at the 1,000,000 RC ceiling". Any other dry-run
// error keeps the old behaviour.
func TestVaultOp_WaitsWhenTheNodeCouldNotReadTheState(t *testing.T) {
	l1 := newFakeL1()
	l2, gql, _, sub, cfg, _ := bridgeWorld(t, l1, 101)
	l2.rc = map[string]int64{sub.did.String(): 2_000_000}
	l2.simErr = "simulateContractCalls: " + nodeStateNotReadable + ", retry"
	_, err := vaultOp(context.Background(), gql, sub, cfg.ContractID, "retireVault", json.RawMessage(`{}`))
	if !errors.Is(err, errDryRunUnreadable) || len(l2.submitted) != 0 {
		t.Fatalf("unreadable state: err %v, %d submissions; want errDryRunUnreadable and nothing sent", err, len(l2.submitted))
	}
	l2.simErr = ""
	if _, err := vaultOp(context.Background(), gql, sub, cfg.ContractID, "retireVault", json.RawMessage(`{}`)); err != nil || len(l2.submitted) != 1 {
		t.Fatalf("another dry-run error: err %v, %d submissions; want the op sent at the ceiling as before", err, len(l2.submitted))
	}
}
