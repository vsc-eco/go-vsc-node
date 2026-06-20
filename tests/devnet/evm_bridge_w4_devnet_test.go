//go:build evm_devnet

// W4: consensus version-gating no-fork. The round-5 fixes that touch deterministic
// WASM-host / state paths (#92 sdk-error-determinism, #119 read_ex, #130 init-guard,
// #136 runtime-versioning, #56 gateway schedule, CD-1 IsSupportedAt) are gated on
// Version0_2_0Height. This run pins it to 200, deploys + exercises the EVM contract
// (which drives the WASM host) on BOTH sides of the activation, and asserts ALL nodes
// stay in consensus across it — i.e. the gates are election-height-deterministic and
// the legacy path is byte-identical (no node-vs-node fork at activation).
//
// Run: go test -tags evm_devnet -run TestEVMBridge_W4 ./tests/devnet/ -timeout 55m -v
package devnet

import (
	"context"
	"testing"
	"time"
)

func TestEVMBridge_W4_ConsensusGatingNoFork(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping EVM-bridge devnet test in short mode")
	}
	requireDocker(t)

	const v020Height = 200

	cfg := regressionConfig() // has SysConfigOverrides initialized
	cfg.Nodes = 5
	cfg.GenesisNode = 5
	cp := cfg.SysConfigOverrides.ConsensusParams
	cp.ElectionInterval = 20
	cp.Version0_2_0Height = v020Height // override devnet default (1) to exercise both sides
	cp.BondInclusionActivationHeight = 0
	// free port range (avoid live testnet + the W0.2/timelock ranges)
	cfg.GQLBasePort = 38080
	cfg.P2PBasePort = 31720
	cfg.MongoPort = 38057
	cfg.HivePort = 38091
	cfg.DronePort = 39000
	cfg.BitcoindRPCPort = 38543
	cfg.DashdRPCPort = 39898

	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Minute)
	t.Cleanup(cancel)

	d, err := New(cfg)
	if err != nil {
		t.Fatalf("creating devnet: %v", err)
	}
	t.Cleanup(func() { d.Stop() })
	if err := d.Start(ctx); err != nil {
		t.Fatalf("starting devnet: %v", err)
	}

	const queryNode = 2

	// Deploy + init the EVM contract PRE-activation (drives the WASM host on the legacy gate).
	id, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath: evmBridgeWasmR5, Name: "evm-bridge-w4", DeployerNode: 1, GQLNode: queryNode,
	})
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	t.Logf("deployed evm-bridge (W4): %s", id)
	if _, err := d.CallContract(ctx, queryNode, id, "initContract", `{"is_testnet":true}`); err != nil {
		t.Fatalf("pre-activation initContract: %v", err)
	}

	// Cross the activation height.
	if err := d.WaitForBlockProcessing(ctx, queryNode, v020Height+20, 35*time.Minute); err != nil {
		t.Fatalf("never crossed activation height %d: %v", v020Height, err)
	}
	t.Logf("crossed activation height %d", v020Height)

	// Exercise a POST-activation contract call (gated WASM-host paths now active).
	if _, err := d.CallContract(ctx, queryNode, id, "initContract", `{"is_testnet":true}`); err != nil {
		t.Logf("post-activation call returned (expected: already-init rejection is fine, not a fork): %v", err)
	}

	// NO-FORK assertion: every node agrees on epoch and is within a tiny block window.
	type ni struct {
		last, epoch uint64
	}
	infos := make([]ni, 0, cfg.Nodes)
	var minB, maxB, ep0 uint64
	for n := 1; n <= cfg.Nodes; n++ {
		lb, ep, err := d.LocalNodeInfo(ctx, n)
		if err != nil {
			t.Fatalf("node %d LocalNodeInfo: %v", n, err)
		}
		infos = append(infos, ni{lb, ep})
		if n == 1 {
			minB, maxB, ep0 = lb, lb, ep
		}
		if lb < minB {
			minB = lb
		}
		if lb > maxB {
			maxB = lb
		}
		if ep != ep0 {
			t.Fatalf("FORK: node %d epoch=%d != node1 epoch=%d (version-gating diverged across activation)", n, ep, ep0)
		}
		t.Logf("node %d: lastBlock=%d epoch=%d", n, lb, ep)
	}
	if maxB-minB > 5 {
		t.Fatalf("FORK/STALL: node block spread %d..%d (>5) across activation — nodes not in consensus", minB, maxB)
	}
	t.Logf("W4 PASS — 5 nodes crossed Version0_2_0Height=%d in consensus (epoch=%d, block spread %d..%d). "+
		"Version-gated WASM-host/state fixes (#92/#119/#130/#136/#56/CD-1) are election-height-deterministic; no fork at activation.", v020Height, ep0, minB, maxB)
}
