package devnet

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestContractUpdateTimelockDevnet validates the 48h contract-update timelock on
// a real multi-node devnet (where the timelock is 30 blocks, not 0 like mocknet):
//
//   - a queued code update does NOT become active immediately; the previously
//     active code keeps running until activation_height (= submit + 30),
//   - the new code activates once the chain head reaches activation_height,
//   - findPendingContractUpdates surfaces the queued update with the right
//     activation height and proposer,
//   - a cancelled update never activates.
//
// All assertions go through the node's GraphQL API (findContract /
// findPendingContractUpdates), exercising the full path: real Hive block
// production -> state-engine timelock -> DB -> resolver.
func TestContractUpdateTimelockDevnet(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet contract-timelock test in short mode")
	}
	requireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	codeAWasm := TestContractPath()
	codeBWasm := filepath.Join(findSourceRoot(), "modules", "e2e", "artifacts", "contract_test2.wasm")
	for _, p := range []string{codeAWasm, codeBWasm} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("test wasm not found: %s: %v", p, err)
		}
	}

	cfg := DefaultConfig()
	// 5 nodes (the default): deploy/update stops the deployer node to take its
	// data dir, so the storage-proof quorum (MinSpSigners=3) needs the remaining
	// nodes to still clear it with margin. 4 nodes leaves exactly 3 and flakes.
	// RAM is not the constraint here — the only spike is the one-time image build;
	// the nodes themselves are light (the watchdog guards the host regardless).
	cfg.Nodes = 5
	cfg.GenesisNode = 5
	// The default devnet ports collide with the live testnet stack on this host;
	// remap every host port to a known-free range.
	cfg.GQLBasePort = 28080
	cfg.P2PBasePort = 21720
	cfg.MongoPort = 28057
	cfg.HivePort = 28091
	cfg.DronePort = 29000
	cfg.BitcoindRPCPort = 28543
	cfg.DashdRPCPort = 29898
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}

	d, err := New(cfg)
	if err != nil {
		t.Fatalf("creating devnet: %v", err)
	}
	t.Cleanup(func() { d.Stop() })
	if err := d.Start(ctx); err != nil {
		t.Fatalf("starting devnet: %v", err)
	}

	const (
		deployNode     = 1
		queryNode      = 2 // never stopped (deployer stops deployNode), so always serves GQL
		timelockBlocks = uint64(30)
	)

	// Poll the active contract until it first appears on-chain.
	waitActive := func(id string) *ContractGQL {
		t.Helper()
		deadline := time.Now().Add(4 * time.Minute)
		for {
			c, err := d.ActiveContract(ctx, queryNode, id)
			if err == nil && c != nil {
				return c
			}
			if time.Now().After(deadline) {
				d.dumpContracts(ctx, t, queryNode)
				t.Fatalf("contract %s never became active (err=%v)", id, err)
			}
			time.Sleep(3 * time.Second)
		}
	}
	// Poll until a pending update appears for the contract.
	waitPending := func(id string) *ContractGQL {
		t.Helper()
		deadline := time.Now().Add(4 * time.Minute)
		for {
			rows, err := d.PendingUpdates(ctx, queryNode, id)
			if err == nil && len(rows) > 0 {
				return &rows[0]
			}
			if time.Now().After(deadline) {
				d.dumpContracts(ctx, t, queryNode)
				t.Fatalf("no pending update appeared for %s (err=%v)", id, err)
			}
			time.Sleep(3 * time.Second)
		}
	}

	// ───────── Scenario 1: queue → old code stays → activates after timelock ─────────
	t.Log("scenario 1: deploy codeA, queue codeB, verify timelock then activation")
	id1, err := d.DeployContract(ctx, ContractDeployOpts{WasmPath: codeAWasm, Name: "timelock-1", DeployerNode: deployNode})
	if err != nil {
		t.Fatalf("deploy contract 1: %v", err)
	}
	a := waitActive(id1)
	codeA := a.Code
	t.Logf("contract %s deployed, active code=%s", id1, codeA)

	if err := d.UpdateContract(ctx, ContractUpdateOpts{ContractId: id1, WasmPath: codeBWasm, Name: "timelock-1", DeployerNode: deployNode}); err != nil {
		t.Fatalf("queue update for %s: %v", id1, err)
	}
	pending := waitPending(id1)
	codeB := pending.Code
	t.Logf("queued update: code=%s creation_height=%d activation_height=%d proposer=%s",
		codeB, pending.CreationHeight, pending.ActivationHeight, pending.Proposer)

	if codeB == codeA {
		t.Fatalf("queued code must differ from active code (both %s)", codeA)
	}
	if got := pending.ActivationHeight - pending.CreationHeight; got != timelockBlocks {
		t.Errorf("timelock window = %d blocks, want %d (creation=%d activation=%d)",
			got, timelockBlocks, pending.CreationHeight, pending.ActivationHeight)
	}
	if pending.Proposer == "" {
		t.Errorf("pending update should record a proposer")
	}

	// During the window the OLD code is still active.
	act, err := d.ActiveContract(ctx, queryNode, id1)
	if err != nil {
		t.Fatalf("active contract during window: %v", err)
	}
	if act.Code != codeA {
		t.Fatalf("during timelock window active code = %s, want old code %s", act.Code, codeA)
	}
	t.Logf("during window: active code still %s ✓", codeA)

	// Wait for the chain to reach activation, then confirm the new code is live.
	if err := d.WaitForBlockProcessing(ctx, queryNode, pending.ActivationHeight+1, 8*time.Minute); err != nil {
		t.Fatalf("waiting for activation height %d: %v", pending.ActivationHeight, err)
	}
	if _, err := d.WaitForActiveCode(ctx, queryNode, id1, codeB, 3*time.Minute); err != nil {
		t.Fatalf("new code did not activate after timelock: %v", err)
	}
	t.Logf("after activation height %d: active code now %s ✓", pending.ActivationHeight, codeB)

	if rows, err := d.PendingUpdates(ctx, queryNode, id1); err != nil {
		t.Fatalf("pending after activation: %v", err)
	} else if len(rows) != 0 {
		t.Errorf("expected no pending updates after activation, got %d", len(rows))
	}

	// ───────── Scenario 2: queue → cancel → never activates ─────────
	t.Log("scenario 2: deploy codeA, queue codeB, cancel, verify it never activates")
	id2, err := d.DeployContract(ctx, ContractDeployOpts{WasmPath: codeAWasm, Name: "timelock-2", DeployerNode: deployNode})
	if err != nil {
		t.Fatalf("deploy contract 2: %v", err)
	}
	waitActive(id2)

	if err := d.UpdateContract(ctx, ContractUpdateOpts{ContractId: id2, WasmPath: codeBWasm, Name: "timelock-2", DeployerNode: deployNode}); err != nil {
		t.Fatalf("queue update for %s: %v", id2, err)
	}
	pending2 := waitPending(id2)
	t.Logf("queued update for %s, activation_height=%d; cancelling...", id2, pending2.ActivationHeight)

	if err := d.CancelContractUpdate(ctx, ContractCancelOpts{ContractId: id2, DeployerNode: deployNode}); err != nil {
		t.Fatalf("cancel update for %s: %v", id2, err)
	}

	// Pending list should drain once the cancel is processed.
	deadline := time.Now().Add(4 * time.Minute)
	for {
		rows, err := d.PendingUpdates(ctx, queryNode, id2)
		if err == nil && len(rows) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pending update for %s was not cancelled (rows=%d err=%v)", id2, len(rows), err)
		}
		time.Sleep(3 * time.Second)
	}
	t.Logf("pending update for %s cancelled ✓", id2)

	// Past the original activation height the OLD code must still be active.
	if err := d.WaitForBlockProcessing(ctx, queryNode, pending2.ActivationHeight+2, 8*time.Minute); err != nil {
		t.Fatalf("waiting past cancelled activation height %d: %v", pending2.ActivationHeight, err)
	}
	act2, err := d.ActiveContract(ctx, queryNode, id2)
	if err != nil {
		t.Fatalf("active contract after cancelled activation: %v", err)
	}
	if act2.Code != codeA {
		t.Fatalf("cancelled update activated: active code = %s, want old code %s", act2.Code, codeA)
	}
	t.Logf("past activation height %d: cancelled update did NOT activate, code still %s ✓", pending2.ActivationHeight, codeA)
}
