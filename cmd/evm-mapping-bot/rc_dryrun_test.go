package main

import (
	"context"
	"encoding/json"
	"testing"

	"vsc-node/modules/common"
	transactionpool "vsc-node/modules/transaction-pool"
)

func submittedRcLimits(t *testing.T, l2 *fakeL2) []uint64 {
	t.Helper()
	l2.mu.Lock()
	defer l2.mu.Unlock()
	var out []uint64
	for _, b := range l2.submitted {
		var shell transactionpool.VSCTransactionShell
		if err := common.DecodeCbor(b, &shell); err != nil {
			t.Fatal(err)
		}
		out = append(out, shell.Headers.RcLimit)
	}
	return out
}

// P6 (BTC bot parity: sizeCallByDryRun on every non-vault call): a map that
// needs more than the flat configured limit is declared at twice its dry run,
// so it does not fail on chain (a map that fails on chain is final).
func TestP6_CallsWithoutALimitAreSizedByDryRun(t *testing.T) {
	cases := []struct {
		name   string
		action string
		sim    func(string) map[string]any
		rc     int64 // bot RC (gas_limit_hit case)
		want   uint64
	}{
		{"needs more than the configured limit", "map", func(string) map[string]any {
			return map[string]any{"success": true, "rc_used": 9000}
		}, 0, 18000},
		{"never lowered", "map", func(string) map[string]any {
			return map[string]any{"success": true, "rc_used": 100}
		}, 0, 10000},
		{"dry run unavailable", "map", nil, 0, 10000},
		{"refused in the dry run", "map", func(string) map[string]any {
			return map[string]any{"success": false, "err": "contract_error", "err_msg": "no"}
		}, 0, 10000},
		// A map over the dry-run cap is no longer sent (BOT-RC-EXHAUST-1,
		// TestBOTRCEXHAUST1_AMapOverTheDryRunCapIsNotSent); every other call
		// keeps this sizing.
		{"over the dry-run cap: what the bot holds", "drainWithdrawalQueue", func(string) map[string]any {
			return map[string]any{"success": false, "err": "gas_limit_hit"}
		}, 500_000, 500_000},
	}
	for _, c := range cases {
		l2, sub := poolWorld(t)
		l2.sim = c.sim
		if c.rc > 0 {
			l2.rc = map[string]int64{sub.did.String(): c.rc}
		}
		if _, err := sub.callContractL2(context.Background(), "vsc1bridge", c.action, json.RawMessage(`{"d":1}`), 0); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := submittedRcLimits(t, l2); len(got) != 1 || got[0] != c.want {
			t.Fatalf("%s: rc_limit %v, want %d", c.name, got, c.want)
		}
	}
}

// A caller's explicit limit (vault ops, MED-99 escalation) is kept as given.
func TestP6_AnExplicitLimitIsKept(t *testing.T) {
	l2, sub := poolWorld(t)
	l2.sim = func(string) map[string]any { return map[string]any{"success": true, "rc_used": 90_000} }
	if _, err := sub.callContractL2(context.Background(), "vsc1bridge", "map", json.RawMessage(`{"d":1}`), 77_777); err != nil {
		t.Fatal(err)
	}
	if got := submittedRcLimits(t, l2); len(got) != 1 || got[0] != 77_777 {
		t.Fatalf("rc_limit %v, want the explicit 77777", got)
	}
}
