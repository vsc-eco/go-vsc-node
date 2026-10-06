package devnet

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/vsc-eco/hivego"
	"go.mongodb.org/mongo-driver/bson"
)

// TestVaultRotationV2MixedVersionUpgrade is the ROLLING-UPGRADE proof for the
// vault-rotation-v2 PR, and it covers the one class of risk no other test in the
// PR touches: the changes that are NOT behind VaultRotationV2ActivationHeight and
// therefore go live on mainnet the moment the binary is deployed, on a fleet that
// is necessarily HETEROGENEOUS for the duration of the rollout.
//
// The PR ships with VaultRotationV2ActivationHeight = 0 on every network, so this
// test deliberately runs with the flag OFF — i.e. exactly the post-merge mainnet
// state. What is live regardless of the flag:
//
//	(1) refreshBtcTheftHalt, the per-block read of the BTC contract's theft flag,
//	    and (2) the vsc.tss_halt op handler. Since 3becf71e both apply only from
//	    the 0.7.0 line (BtcKeysignHaltActive), so below it (this fleet, and
//	    mainnet's rollout at 0.3.0) new nodes ignore them like old nodes do.
//	(3) consensus_unstake ledger records now always carry Params["from"].
//
// Each of those makes NEW nodes do work OLD nodes do not. The question this test
// answers is the only one that matters for the deploy: does that asymmetry FORK
// the chain (nodes disagree on a block) or STALL it (block production stops)?
// The assertion throughout is cross-node convergence of block_headers — every
// node's view of every VSC block slot must be byte-identical.
//
// Layout: 6 nodes, 1-3 on the PR code, 4-6 on the PR's merge-base (cc069b3f).
// The gateway multisig threshold on a 6-node devnet is 6*2/3 = 4.
//
// Run:
//
//	VAULT_MIXED_RUN=1 OLD_CODE_DIR=/home/dockeruser/magi/testnet/key_rotation/go-vsc-node-base \
//	  go test -v -run TestVaultRotationV2MixedVersionUpgrade -timeout 45m ./tests/devnet/
func TestVaultRotationV2MixedVersionUpgrade(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("VAULT_MIXED_RUN") == "" {
		t.Skip("set VAULT_MIXED_RUN=1 to run the mixed-version rolling-upgrade test")
	}
	requireDocker(t)

	oldCodeDir := os.Getenv("OLD_CODE_DIR")
	if oldCodeDir == "" {
		oldCodeDir = "/home/dockeruser/magi/testnet/key_rotation/go-vsc-node-base"
	}
	if _, err := os.Stat(oldCodeDir); err != nil {
		t.Fatalf("old-code repo (the PR merge-base) not found at %s: %v", oldCodeDir, err)
	}
	wasm := os.Getenv("BTC_MAPPING_WASM_PATH")
	if wasm == "" {
		t.Fatal("BTC_MAPPING_WASM_PATH must point at the btc-mapping-contract regtest wasm")
	}

	ctx, cancel := context.WithTimeout(context.Background(), vfTestBudget(42*time.Minute))
	defer cancel()

	newNodes := []int{1, 2, 3}
	oldNodes := []int{4, 5, 6}

	cfg := tssTestConfig()
	cfg.Nodes = 6
	cfg.SkipFunding = false
	cfg.EnableBitcoind = true
	cfg.GenesisNode = 1 // genesis on a new-code node
	cfg.OldCodeSourceDir = oldCodeDir
	cfg.OldCodeNodes = oldNodes
	// The old nodes must run the same network parameters as the new ones; without
	// -sysconfig they ran devnet defaults (other rotate interval, timeouts, election
	// interval), so the two halves started TSS sessions at different heights.
	cfg.OldCodeSysconfig = true
	// The old-code image's default Go base is 1.24.1, but the merge-base go.mod
	// requires >= 1.25.7 (GOTOOLCHAIN=local in the image, so it won't self-upgrade).
	cfg.OldCodeGoImage = "golang:1.25.10"
	// v2 OFF — the mainnet post-merge state. The whole point of the test.
	cfg.SysConfigOverrides.ConsensusParams.VaultRotationV2ActivationHeight = 0
	// MV-03 signs vsc.gateway ops with keys it re-derives itself, which match the
	// authority devnet-setup installs only with deterministic seeds (as TssHalt, F16).
	cfg.MagiEnv = map[string]string{"DEVNET_DETERMINISTIC_BLS": "1"}
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}

	d, _ := startDevnetNoKey(t, cfg, 40*time.Minute)
	t.Logf("mixed fleet up: new-code=%v (PR) old-code=%v (merge-base cc069b3f)", newNodes, oldNodes)

	// ── Wire the BTC contract, which is what ARMS the ungated per-block
	// refreshBtcTheftHalt read on the new nodes. Without ContractId("BTC") set,
	// that code path is inert and the test proves nothing.
	cid, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath: wasm, Name: "btc-mapping-contract", Description: "mixed-version", DeployerNode: 1, GQLNode: 2,
	})
	if err != nil {
		t.Fatalf("deploying btc-mapping-contract: %v", err)
	}
	t.Logf("btc-mapping deployed: %s", cid)

	if err := d.SetOracleContractIDs(map[string]string{"BTC": cid}); err != nil {
		t.Fatalf("setting ChainContracts[BTC]: %v", err)
	}
	if err := d.RestartAllMagiNodes(ctx); err != nil {
		t.Fatalf("restarting magi nodes: %v", err)
	}
	t.Log("ChainContracts[BTC] wired — refreshBtcTheftHalt is now live on the new nodes, every block")

	// ── CASE MV-01: the mixed fleet converges and keeps producing.
	// This is the baseline no-fork/no-stall assertion with the ungated per-block
	// contract read active on half the fleet.
	base := requireConverged(t, ctx, d, cfg.Nodes, 0, 6*time.Minute)
	t.Logf("CASE MV-01 PASS — 6-node mixed fleet converged at block %d (no fork from the per-block theft-halt read)", base)

	// ── CASE MV-02: consensus_unstake on the mixed fleet.
	// ledger-system/utils.go now unconditionally writes Params["from"] onto the
	// unstake action record. Old nodes don't. If that field is hashed into any
	// committed root, this forks; if it isn't, the fleet stays converged.
	if _, err := d.ConsensusUnstake(2, "1.000"); err != nil {
		t.Fatalf("consensus_unstake broadcast: %v", err)
	}
	after := requireConverged(t, ctx, d, cfg.Nodes, base+20, 6*time.Minute)
	t.Logf("CASE MV-02 PASS — consensus_unstake (new Params[\"from\"]) applied, fleet still converged at %d", after)

	// ── CASE MV-03: the ungated vsc.tss_halt op on a heterogeneous fleet.
	// New nodes must set consensus_state.btc_keysign_halted; old nodes have no
	// handler and must ignore the op. The fleet must NOT fork and must NOT stall.
	// Gateway multisig: derive one key per witness from the SAME prefix the devnet
	// actually uses (hardcoding the account names silently produces keys that don't
	// satisfy vsc.gateway's active authority). Threshold on 6 nodes is 6*2/3 = 4.
	var allKeys []*hivego.KeyPair
	for n := 1; n <= cfg.Nodes; n++ {
		allKeys = append(allKeys, devnetGatewayKeypair(t, fmt.Sprintf("%s%d", cfg.WitnessPrefix, n)))
	}
	signWith := allKeys[:4]
	// devnet-setup creates vsc.gateway with every witness's gateway key at weight 1.
	// Retry briefly in case the first broadcast races the chain.
	payload, _ := json.Marshal(map[string]any{"active": true, "keyId": cid + "-main"})
	var haltTx string
	deadline := time.Now().Add(25 * time.Minute)
	for time.Now().Before(deadline) {
		haltTx, err = broadcastGatewayMultisig(t, d, "vsc.tss_halt", []string{"vsc.gateway"}, string(payload), signWith)
		if err == nil {
			break
		}
		t.Logf("  vsc.gateway authority not live yet (%v) — retrying", firstLine(err.Error()))
		time.Sleep(20 * time.Second)
	}
	if err != nil {
		// Withdrawals through the gateway multisig work on a mixed fleet
		// (TestGatewayMixedFleet), so an authority that never goes live here is a failure.
		t.Errorf("CASE MV-03 FAIL: vsc.gateway authority not usable on the mixed fleet within 25m; last error: %v", firstLine(err.Error()))
		return
	}
	t.Logf("vsc.tss_halt (active=true) broadcast: %s", haltTx)

	halted := requireConverged(t, ctx, d, cfg.Nodes, after+20, 6*time.Minute)

	// Since 3becf71e the op only applies from the 0.7.0 line (BtcKeysignHaltActive).
	// This fleet runs below it, as mainnet's rolling upgrade does at 0.3.0, so NEW
	// nodes must ignore the op exactly like the old nodes (which have no handler):
	// no halt anywhere, no asymmetry, and the fleet stays converged.
	newHalted := countHaltFlag(t, ctx, d, newNodes)
	oldHalted := countHaltFlag(t, ctx, d, oldNodes)
	switch {
	case newHalted != 0:
		t.Errorf("CASE MV-03 FAIL: %d/%d NEW nodes set btc_keysign_halted below the 0.7.0 line (the op must be ignored there, as on the old nodes)", newHalted, len(newNodes))
	case oldHalted != 0:
		t.Errorf("CASE MV-03 FAIL: %d OLD nodes set btc_keysign_halted (they have no handler; impossible)", oldHalted)
	default:
		t.Logf("CASE MV-03 PASS: below the 0.7.0 line the halt is ignored by new and old nodes alike (0/%d new, 0/%d old), fleet converged at %d (no fork, no stall)",
			len(newNodes), len(oldNodes), halted)
	}

	// ── CASE MV-04: clear the halt. Proves the flag is recoverable on a mixed
	// fleet (governance can un-freeze without waiting for the fleet to converge
	// on a version).
	payload, _ = json.Marshal(map[string]any{"active": false, "keyId": cid + "-main"})
	if _, err := broadcastGatewayMultisig(t, d, "vsc.tss_halt", []string{"vsc.gateway"}, string(payload), signWith); err != nil {
		t.Fatalf("broadcasting vsc.tss_halt clear: %v", err)
	}
	cleared := requireConverged(t, ctx, d, cfg.Nodes, halted+20, 6*time.Minute)
	if n := countHaltFlag(t, ctx, d, newNodes); n != 0 {
		t.Errorf("CASE MV-04 FAIL — %d/%d new nodes still halted after active=false", n, len(newNodes))
	} else {
		t.Logf("CASE MV-04 PASS — halt cleared on all new nodes, fleet converged at %d", cleared)
	}
}

// requireConverged waits until every node has processed past minBlock, then
// asserts that all nodes agree, block-for-block, on every VSC block slot they
// have in common. A single differing block CID at the same slot height is a
// FORK — the failure mode this whole test exists to detect. Returns the common
// height reached.
func requireConverged(t *testing.T, ctx context.Context, d *Devnet, nodes int, minBlock uint64, timeout time.Duration) uint64 {
	t.Helper()

	deadline := time.Now().Add(timeout)
	var common uint64
	for time.Now().Before(deadline) {
		lowest := ^uint64(0)
		ok := true
		for n := 1; n <= nodes; n++ {
			bh, err := d.getLastProcessedBlock(ctx, n)
			if err != nil || bh <= minBlock {
				ok = false
				break
			}
			if bh < lowest {
				lowest = bh
			}
		}
		if ok {
			common = lowest
			break
		}
		time.Sleep(5 * time.Second)
	}
	if common == 0 {
		// Not all nodes advanced past minBlock — a STALL, which is itself a
		// deploy-blocking outcome, so fail rather than skip.
		for n := 1; n <= nodes; n++ {
			bh, err := d.getLastProcessedBlock(ctx, n)
			t.Logf("  magi-%d last_processed_block=%d err=%v", n, bh, err)
		}
		t.Fatalf("fleet did not all advance past block %d within %s — STALL", minBlock, timeout)
	}

	// Cross-node block-for-block comparison.
	client, err := d.mongoClient(ctx)
	if err != nil {
		t.Fatalf("mongo: %v", err)
	}
	defer client.Disconnect(ctx)

	type blk struct {
		SlotHeight int    `bson:"slot_height"`
		Block      string `bson:"block"`
	}
	ref := map[int]string{}
	compared := 0
	for n := 1; n <= nodes; n++ {
		cur, err := client.Database(d.nodeDbName(n)).Collection("block_headers").
			Find(ctx, bson.M{})
		if err != nil {
			t.Fatalf("reading block_headers from magi-%d: %v", n, err)
		}
		var rows []blk
		if err := cur.All(ctx, &rows); err != nil {
			t.Fatalf("decoding block_headers from magi-%d: %v", n, err)
		}
		for _, r := range rows {
			prev, seen := ref[r.SlotHeight]
			if !seen {
				ref[r.SlotHeight] = r.Block
				continue
			}
			compared++
			if prev != r.Block {
				t.Fatalf("FORK — slot %d: magi-%d has block %s, an earlier node has %s",
					r.SlotHeight, n, r.Block, prev)
			}
		}
	}
	t.Logf("  converged: common height %d, %d VSC blocks known, %d cross-node block comparisons all identical",
		common, len(ref), compared)
	return common
}

// countHaltFlag returns how many of the given nodes have
// consensus_state.btc_keysign_halted == true.
func countHaltFlag(t *testing.T, ctx context.Context, d *Devnet, nodes []int) int {
	t.Helper()
	client, err := d.mongoClient(ctx)
	if err != nil {
		t.Fatalf("mongo: %v", err)
	}
	defer client.Disconnect(ctx)

	n := 0
	for _, node := range nodes {
		var doc struct {
			Halted bool   `bson:"btc_keysign_halted"`
			Height uint64 `bson:"btc_keysign_halt_height"`
		}
		err := client.Database(d.nodeDbName(node)).Collection("chain_consensus_state").
			FindOne(ctx, bson.M{"_id": "singleton"}).Decode(&doc)
		if err != nil {
			t.Logf("  magi-%d: no consensus_state doc (%v)", node, err)
			continue
		}
		t.Logf("  magi-%d: btc_keysign_halted=%v height=%d", node, doc.Halted, doc.Height)
		if doc.Halted {
			n++
		}
	}
	return n
}

func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	if len(s) > 120 {
		return s[:120]
	}
	return s
}
