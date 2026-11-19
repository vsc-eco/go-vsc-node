package devnet

// Saving a contract's state after a call that writes many keys, on a real
// devnet: block processing keeps its pace and every node stays in step. This
// is the network-level check of the datalayer's batched block writes
// (lib/datalayer/batch_dag.go); it does not depend on the write price.
//
//	go test -v -run 'TestHardening_ManyKeysInOneCall' -timeout 150m ./tests/devnet/

import (
	"context"
	"fmt"
	"testing"
	"time"

	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"
)

// hardeningDevnet is a 5-node devnet on consensus 0.16 with the probe contract
// deployed and every witness holding 1,080 HBD on the ledger (1,090,000 RC).
func hardeningDevnet(t *testing.T, budget time.Duration) (*Devnet, context.Context, string) {
	t.Helper()
	requireDocker(t)
	wasm := buildRcProbeContract(t)

	cfg := DefaultConfig()
	cfg.Nodes = 5
	cfg.LogLevel = "info"
	cfg.SysConfigOverrides = &systemconfig.SysConfigOverrides{
		ConsensusParams: &params.ConsensusParams{
			ElectionInterval:               20,
			ConsensusVersionFloorEpoch:     1,
			ConsensusVersionFloorMajor:     0,
			ConsensusVersionFloorConsensus: 16,
		},
	}
	d, ctx := startDevnetNoKey(t, cfg, vfTestBudget(budget))
	if err := d.WaitForBlockProcessing(ctx, 1, 30, 8*time.Minute); err != nil {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("network never reached block 30: %v", err)
	}
	if err := d.waitForElectionEpoch(ctx, 1, 1, 10*time.Minute); err != nil {
		t.Fatalf("first election never indexed: %v", err)
	}
	if err := d.waitForRcBackfill(ctx); err != nil {
		t.Fatalf("witness RC funding: %v", err)
	}
	for n := 1; n <= cfg.Nodes; n++ {
		if _, err := d.Deposit(ctx, n, "90.000", "hbd"); err != nil {
			t.Fatalf("deposit for %s: %v", d.witnessAccount(n), err)
		}
	}
	for n := 1; n <= cfg.Nodes; n++ {
		if !waitForL2Balance(t, d, ctx, 1, "hive:"+d.witnessAccount(n), "hbd", 1_080_000, 5*time.Minute) {
			t.Fatalf("%s never held 1,080 HBD on the ledger", d.witnessAccount(n))
		}
	}
	cid, err := d.DeployContract(ctx, ContractDeployOpts{WasmPath: wasm, Name: "rcprobe", DeployerNode: 1, GQLNode: 2})
	if err != nil {
		t.Fatalf("deploy the probe contract: %v", err)
	}
	if err := d.WaitForBlockProcessing(ctx, 2, nodeBlock(t, d, ctx, 1), 3*time.Minute); err != nil {
		t.Fatalf("nodes not synced after the deploy: %v", err)
	}
	return d, ctx, cid
}

// A call that creates many keys: the state save after it keeps block
// processing at its pace, and a later write to the same contract does too.
func TestHardening_ManyKeysInOneCallKeepBlockPace(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet hardening test in short mode")
	}
	d, ctx, cid := hardeningDevnet(t, 110*time.Minute)
	const nodes = 5
	const keys = 60_000
	const longestPauseAllowed = 60 * time.Second

	// watch reports the longest time magi-1 went without processing a block
	// over the next `span`, in real time (what an operator sees), whatever the
	// block cadence.
	watch := func(span time.Duration) time.Duration {
		last, _ := nodeBlockOpt(d, ctx, 1)
		start := time.Now() // wall: the pause is measured in real time
		lastMove := start
		var longest time.Duration
		for time.Since(start) < span { // wall: the pause is measured in real time
			time.Sleep(2 * time.Second) // wall: the pause is measured in real time
			cur, ok := nodeBlockOpt(d, ctx, 1)
			if ok && cur > last {
				last, lastMove = cur, time.Now() // wall: the pause is measured in real time
				continue
			}
			if p := time.Since(lastMove); p > longest { // wall: the pause is measured in real time
				longest = p
			}
		}
		return longest
	}

	baseline := watch(90 * time.Second)
	t.Logf("before the call: the longest pause between blocks on magi-1 was %s", baseline.Round(time.Second))

	t.Run(fmt.Sprintf("01_%d_keys_in_one_call_keep_block_pace", keys), func(t *testing.T) {
		o := rcpCallNoWait(t, d, d.witnessAccount(2), cid, "manyKeys", fmt.Sprintf("k:%d:0", keys), 1_000_000)
		longest := watch(8 * time.Minute)
		st := rcpWaitFinal(d, ctx, 1, o, 6*time.Minute)
		t.Logf("%d keys in one call: status %q; the longest pause between blocks on magi-1 was %s (before the call: %s)", keys, st, longest.Round(time.Second), baseline.Round(time.Second))
		if st != "CONFIRMED" {
			t.Fatalf("the call is %q, want CONFIRMED", st)
		}
		if longest > longestPauseAllowed {
			t.Fatalf("after one call that created %d keys the longest pause between blocks was %s (allowed %s)", keys, longest.Round(time.Second), longestPauseAllowed)
		}
	})

	t.Run("02_a_later_write_to_that_contract_keeps_block_pace", func(t *testing.T) {
		o := rcpCallNoWait(t, d, d.witnessAccount(3), cid, "setString", "one,more", 100_000)
		longest := watch(3 * time.Minute)
		st := rcpWaitFinal(d, ctx, 1, o, 6*time.Minute)
		t.Logf("one more key on a contract with %d keys: status %q; longest pause %s", keys, st, longest.Round(time.Second))
		if st != "CONFIRMED" || longest > longestPauseAllowed {
			t.Fatalf("status %q, longest pause %s (allowed %s)", st, longest.Round(time.Second), longestPauseAllowed)
		}
	})

	t.Run("03_every_node_is_still_in_step", func(t *testing.T) {
		head := nodeBlock(t, d, ctx, 1)
		for n := 1; n <= nodes; n++ {
			if err := d.WaitForBlockProcessing(ctx, n, head+10, 8*time.Minute); err != nil {
				dumpDiagnostics(t, d, ctx)
				t.Fatalf("magi-%d fell behind: %v", n, err)
			}
		}
	})
}

// rcpCallNoWait sends a call and returns its transaction id without waiting.
func rcpCallNoWait(t *testing.T, d *Devnet, account, cid, action, payload string, rcLimit int) string {
	t.Helper()
	txId, err := rcpCall(d, account, cid, action, payload, rcLimit, nil)
	if err != nil {
		t.Fatalf("%s as %s: %v", action, account, err)
	}
	return txId
}
