//go:build evm_devnet

package devnet

// Wallet accounts (an Ethereum or Bitcoin key signing its own transactions)
// before and after the floor reaches consensus 0.15.0. A wallet account has no
// free RC: its RC is the HBD it holds. Its transactions go through the
// transaction pool, which refuses a declared rc_limit above the account's RC,
// and through the block producer, which budgets each transaction by its limit.
//
//	go test -tags evm_devnet -run TestRcPrice_WalletAccounts ./tests/devnet/ -timeout 150m -v

import (
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"

	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

func TestRcPrice_WalletAccounts(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet write price test in short mode")
	}
	requireDocker(t)
	wasm := buildRcProbeContract(t)

	const nodes = 5
	cfg := DefaultConfig()
	cfg.Nodes = nodes
	cfg.LogLevel = "info"
	cfg.SysConfigOverrides = &systemconfig.SysConfigOverrides{
		ConsensusParams: &params.ConsensusParams{
			ElectionInterval:               20,
			ConsensusVersionFloorEpoch:     1,
			ConsensusVersionFloorMajor:     0,
			ConsensusVersionFloorConsensus: 14,
		},
	}
	d, ctx := startDevnetNoKey(t, cfg, vfTestBudget(140*time.Minute))
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
	cid, err := d.DeployContract(ctx, ContractDeployOpts{WasmPath: wasm, Name: "rcprobe", DeployerNode: 1, GQLNode: 2})
	if err != nil {
		t.Fatalf("deploy the probe contract: %v", err)
	}
	if err := d.WaitForBlockProcessing(ctx, 2, nodeBlock(t, d, ctx, 1), 3*time.Minute); err != nil {
		t.Fatalf("nodes not synced after the deploy: %v", err)
	}
	rcpSettleAfterDeploy(t, d, ctx, nodes)

	newWallet := func() *EthKey {
		k, err := NewEthKey(hex.EncodeToString(ethCrypto.FromECDSA(mustKey(t))))
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	// fund moves HBD on the ledger from witness 2 to a wallet account.
	fund := func(k *EthKey, hbd string, wantMilli int64) {
		t.Helper()
		payload := map[string]any{"from": "hive:" + d.witnessAccount(2), "to": k.DID.String(), "amount": hbd, "asset": "hbd", "net_id": d.netId()}
		if _, err := d.BroadcastCustomJSON("vsc.transfer", []string{d.witnessAccount(2)}, payload, d.cfg.InitminerWIF); err != nil {
			t.Fatalf("funding %s: %v", k.DID.String(), err)
		}
		if !waitForL2Balance(t, d, ctx, 2, k.DID.String(), "hbd", wantMilli, 5*time.Minute) {
			t.Fatalf("%s never held %s HBD", k.DID.String(), hbd)
		}
	}
	// walletCall submits one signed call and returns its status and what the
	// chain charged the wallet account for it.
	walletCall := func(t *testing.T, k *EthKey, action, payload string, rcLimit uint64) rcpOutcome {
		t.Helper()
		payer := k.DID.String()
		prev, havePrev, err := rcpLatest(ctx, d, rcpRefNode, payer)
		if err != nil {
			t.Fatalf("reading %s RC record: %v", payer, err)
		}
		txId, err := d.CallContractEvm(ctx, 2, k, cid, action, payload, rcLimit)
		if err != nil {
			t.Fatalf("%s(%s) as %s: %v", action, rcpShort(payload), payer, err)
		}
		out := rcpOutcome{TxId: txId}
		out.Status = rcpWaitFinal(d, ctx, rcpRefNode, txId, 8*time.Minute)
		if out.Status == "" {
			t.Fatalf("%s(%s) as %s never became final (tx %s)", action, rcpShort(payload), payer, txId)
		}
		out.Used = rcpWaitCharge(t, d, ctx, payer, prev, havePrev)
		t.Logf("%s(%s) as a wallet account: %s, charged %d RC (rc_limit %d)", action, rcpShort(payload), out.Status, out.Used, rcLimit)
		return out
	}
	must := func(name string, f func(t *testing.T)) {
		t.Helper()
		if !t.Run(name, f) {
			dumpDiagnostics(t, d, ctx)
			t.Fatalf("%s failed; the cases after it depend on it", name)
		}
	}

	const held = 6000 // RC each wallet account holds: 6 HBD
	const size = 400  // new bytes it tries to store: 7,600 RC at 19, 400 at 1
	empty, early, late := newWallet(), newWallet(), newWallet()
	fund(early, "6.000", held)
	fund(late, "6.000", held)

	must("before/01_the_chain_is_on_0.14", func(t *testing.T) {
		if err := pollAllNodes(nodes, 8*time.Minute, func(n int) bool {
			line, _, _, e := d.ConsensusInfo(ctx, n)
			return e == nil && line == "0.14"
		}); err != nil {
			t.Fatalf("not every node is on an active 0.14: %v", err)
		}
	})

	t.Run("before/02_a_wallet_account_without_hbd_has_no_rc_and_the_pool_refuses_it", func(t *testing.T) {
		_, err := d.CallContractEvm(ctx, 2, empty, cid, "noop", "x", 100)
		if err == nil || !strings.Contains(err.Error(), "not enough RCS") {
			t.Fatalf("a call from an account with no HBD: err=%v, want the pool to refuse it for RC", err)
		}
	})

	t.Run("before/03_the_pool_refuses_a_limit_above_the_accounts_rc", func(t *testing.T) {
		_, err := d.CallContractEvm(ctx, 2, early, cid, "noop", "x", held+1000)
		if err == nil || !strings.Contains(err.Error(), "not enough RCS") {
			t.Fatalf("rc_limit %d with %d RC held: err=%v, want the pool to refuse it", held+1000, held, err)
		}
	})

	must("before/04_400_new_bytes_cost_more_than_6_hbd_of_rc", func(t *testing.T) {
		o := walletCall(t, early, "setBlob", fmt.Sprintf("w1:%d", size), held)
		if o.Status != "FAILED" {
			t.Fatalf("storing %d new bytes with %d RC: status %s, want FAILED (it costs %d at 19 a byte)", size, held, o.Status, size*19)
		}
		// The whole write is charged, and the frozen amount stops at what the
		// account holds: it is left with no RC at all.
		if o.Used != held {
			t.Fatalf("the failed call froze %d RC, want everything the account holds (%d)", o.Used, held)
		}
	})

	must("switch/05_the_proposal_raises_the_floor_to_0.15", func(t *testing.T) {
		rcpRaiseFloor(t, d, ctx, nodes, 15, 40*time.Minute)
		if err := pollAllNodes(nodes, 10*time.Minute, func(n int) bool {
			line, _, _, e := d.ConsensusInfo(ctx, n)
			return e == nil && line == "0.15"
		}); err != nil {
			t.Fatalf("not every node adopted 0.15: %v", err)
		}
	})

	var storeCost int64
	t.Run("after/06_the_same_400_bytes_pass_with_the_same_6_hbd", func(t *testing.T) {
		o := walletCall(t, late, "setBlob", fmt.Sprintf("w2:%d", size), held)
		if o.Status != "CONFIRMED" {
			t.Fatalf("status %s, want CONFIRMED: %d new bytes cost about %d RC at 1 a byte", o.Status, size, size)
		}
		if o.Used < size || o.Used > size+400 {
			t.Fatalf("charged %d RC, want %d to %d", o.Used, size, size+400)
		}
		storeCost = o.Used
	})

	t.Run("after/07_the_pool_still_refuses_a_limit_above_the_accounts_rc", func(t *testing.T) {
		_, err := d.CallContractEvm(ctx, 2, late, cid, "noop", "x", held)
		if err == nil || !strings.Contains(err.Error(), "not enough RCS") {
			t.Fatalf("rc_limit %d with %d RC left: err=%v, want the pool to refuse it", held, held-storeCost, err)
		}
	})

	t.Run("after/08_a_wallet_call_that_runs_out_is_charged_its_limit", func(t *testing.T) {
		const limit = 1000
		o := walletCall(t, late, "logBlob", "50000", limit)
		if o.Status != "FAILED" || o.Used != limit {
			t.Fatalf("status %s, charged %d RC, want FAILED and exactly the limit %d", o.Status, o.Used, limit)
		}
	})

	t.Run("after/09_the_account_keeps_the_rest_of_its_rc", func(t *testing.T) {
		// 6,000 held, about 400 and 1,000 frozen: a call under what is left passes.
		o := walletCall(t, late, "noop", "x", 3000)
		if o.Status != "CONFIRMED" || o.Used != 100 {
			t.Fatalf("status %s, charged %d RC, want CONFIRMED and the 100 RC minimum", o.Status, o.Used)
		}
	})

	t.Run("after/10_every_node_holds_the_same_rc_records", func(t *testing.T) {
		rcpNodesAgreeOnRc(t, d, ctx, []int{1, 2, 3, 4, 5}, []string{early.DID.String(), late.DID.String()})
	})
}
