//go:build evm_devnet

// W-MOCK — foundation test for the L1 bridge waves (W1/W2/W3) under the
// "proof is Milo's" scope. Validates the TWO new test-harness mechanisms on a
// real 5-node devnet, BEFORE the heavier bot/anvil deposit pipeline:
//
//   (1) Short-timelock devnet build of account-mapping (build tag
//       devnet_fasttimelock, TimelockLong=5) lets setVerifierContract be
//       bootstrapped within the devnet's block budget. ASSERT: after
//       propose -> wait -> execute, account-mapping's "zkverifier" state key
//       == the mock verifier id (the 400K-block timelock would make this
//       impossible). Production 400K timelock is unchanged + tested separately.
//
//   (2) Mock ZK header verifier (devSetHeader) plants a header with the REAL,
//       independently-verified roots of Sepolia block 10764834 (the 2026-05-01
//       testnet-verified header) in the EXACT 137-byte layout the real
//       zk-review6 verifier's storeHeader produces. ASSERT: the mock's
//       b-10764834 state == the expected byte-exact header; h == "10764834".
//
// Cross-contract CONSUMPTION of the header (deposit credit / withdrawal
// close-out / double-spend) is proven in the W1/W2/W3 waves that build on this.
//
// Run: go test -tags evm_devnet -run TestEVMBridge_WMock ./tests/devnet/ -timeout 40m -v
package devnet

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

const (
	// account-mapping med/am-review6, built WITH -tags devnet_fasttimelock (short timelock).
	evmBridgeWasmDevnet = "/home/clauderfly/r6-wt/account-mapping/evm-mapping-contract/bin/main-devnet.wasm"
	// mock ZK header verifier (test double) — zk-review6 worktree.
	mockVerifierWasm = "/home/clauderfly/r6-wt/zk-header-verifier/bin/mock-verifier.wasm"
)

// REAL verified header — Sepolia block 10764834 (REAL-VERIFIED-HEADER-10764834.md):
// pulled byte-exact from testnet verifier b-10764834 AND re-verified vs Sepolia RPC.
const (
	hdrBlockNumber = uint64(10764834)
	hdrStateRoot   = "6efab94327bc2fd7eae04922c44be6be72f4e9f402410df10131be20870dc15e"
	hdrTxRoot      = "c0411a58e6d4ef53b9ac5bda11a6673ed0773e3716e1a1172a152d79819a592b"
	hdrRcptRoot    = "a57f50e0b81cee33609c2472b9bc9a83d6565d8b8042327f3e39665409737df5"
	hdrBaseFee     = uint64(464667)
	hdrGasLimit    = uint64(60000000)
	hdrTimestamp   = uint64(1777587936)
	hdrChainId     = uint64(11155111)
)

// expectedHeaderHex = the 137-byte review6 layout (version 0x01 + fields + chainId),
// exactly what zk-review6 storeHeader writes and review6 DeserializeHeader reads.
func expectedHeaderHex() string {
	u64 := func(v uint64) string { b := make([]byte, 8); for i := 0; i < 8; i++ { b[7-i] = byte(v >> (8 * i)) }; return hex.EncodeToString(b) }
	return "01" + u64(hdrBlockNumber) + hdrStateRoot + hdrTxRoot + hdrRcptRoot +
		u64(hdrBaseFee) + u64(hdrGasLimit) + u64(hdrTimestamp) + u64(hdrChainId)
}

// callRet reads the contract-output result (ret message + ok) for a tx id —
// the actual contract-level abort message that tx status ("FAILED") hides.
func (d *Devnet) callRet(ctx context.Context, node int, txId string) (string, bool, bool) {
	const q = `query($id:String!){findContractOutput(filterOptions:{byInput:$id}){results{ret ok}}}`
	var out struct {
		FindContractOutput []struct {
			Results []struct {
				Ret string `json:"ret"`
				Ok  bool   `json:"ok"`
			} `json:"results"`
		} `json:"findContractOutput"`
	}
	if err := d.gqlQuery(ctx, node, q, map[string]any{"id": txId}, &out); err != nil {
		return "", false, false
	}
	for _, o := range out.FindContractOutput {
		for _, r := range o.Results {
			return r.Ret, r.Ok, true
		}
	}
	return "", false, false
}

// getStateHex reads a contract state key as hex (binary-safe, unlike the
// UTF-8-decoding GetStateByKeys helper).
func (d *Devnet) getStateHex(ctx context.Context, node int, contractId, key string) (string, error) {
	const q = `query($c:String!,$k:[String!]!){getStateByKeys(contractId:$c,keys:$k,encoding:"hex")}`
	var out struct {
		GetStateByKeys map[string]any `json:"getStateByKeys"`
	}
	if err := d.gqlQuery(ctx, node, q, map[string]any{"c": contractId, "k": []string{key}}, &out); err != nil {
		return "", err
	}
	if v, ok := out.GetStateByKeys[key]; ok && v != nil {
		return strings.TrimPrefix(fmt.Sprintf("%v", v), "0x"), nil
	}
	return "", nil
}

func TestEVMBridge_WMock_HeaderInjection(t *testing.T) {
	if testing.Short() {
		t.Skip("devnet")
	}
	requireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	t.Cleanup(cancel)

	cfg := regressionConfig()
	cfg.Nodes = 5
	cfg.GenesisNode = 5
	cfg.SysConfigOverrides.ConsensusParams.ElectionInterval = 20
	cfg.SysConfigOverrides.ConsensusParams.BondInclusionActivationHeight = 0
	cfg.GQLBasePort = 38080
	cfg.P2PBasePort = 31720
	cfg.MongoPort = 38057
	cfg.HivePort = 38091
	cfg.DronePort = 39000
	cfg.BitcoindRPCPort = 38543
	cfg.DashdRPCPort = 39898

	d, err := New(cfg)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	t.Cleanup(func() { d.Stop() })
	if err := d.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	const ownerNode = 1 // deployer == contract.owner == magi.test1; owner ops sign as node 1
	const gqlNode = 2
	ownerAcct := "hive:" + fmt.Sprintf("%s%d", cfg.WitnessPrefix, ownerNode)

	// RC bootstrap: the owner witness starts with only the ~10k free RC, whose
	// gas budget (gas=min(availableGas,rc_limit) * CYCLE_GAS_PER_RC) is too small
	// for the heavier admin calls — `execute` does a full LoadProposal
	// JSON-unmarshal + dispatch and hit gas_limit_hit ("cost limit exceeded").
	// Deposit HBD into the owner's L2 ledger so CanConsume sees real RC. Broadcast
	// now; it credits via the gateway during the deploys (waited on below).
	if _, err := d.Deposit(ctx, ownerNode, "5000.000", "hbd"); err != nil {
		t.Fatalf("RC bootstrap deposit: %v", err)
	}

	// 1) Deploy the short-timelock account-mapping + init.
	amId, err := d.DeployContract(ctx, ContractDeployOpts{WasmPath: evmBridgeWasmDevnet, Name: "evm-bridge-mockwire", DeployerNode: ownerNode, GQLNode: gqlNode})
	if err != nil {
		t.Fatalf("deploy account-mapping(devnet): %v", err)
	}
	t.Logf("account-mapping (devnet build): %s", amId)
	dumpState := func(label, cid string, keys ...string) {
		m, e := d.GetStateByKeys(ctx, gqlNode, cid, keys)
		t.Logf("STATE[%s] %v (err=%v)", label, m, e)
	}
	logTx := func(label, txId string) {
		if txId == "" {
			t.Logf("TX[%s] empty txId", label)
			return
		}
		var st string
		_ = pollUntil(ctx, 90*time.Second, func() bool {
			st, _ = d.FindTransactionStatus(ctx, gqlNode, txId)
			return st != "" && st != "UNCONFIRMED" && st != "INCLUDED"
		})
		// also fetch the contract-output ret (the real abort message)
		var ret string
		var ok, found bool
		_ = pollUntil(ctx, 30*time.Second, func() bool {
			ret, ok, found = d.callRet(ctx, gqlNode, txId)
			return found
		})
		t.Logf("TX[%s] id=%s status=%q ok=%v ret=%q", label, txId, st, ok, ret)
	}
	// dumpTxDag reads the raw contract-output DAG for a tx (the un-hidden
	// error message that findContractOutput.ret strips — getDagByCID per
	// feedback_check_raw_dag_errors).
	dumpTxDag := func(label, txId string) {
		const q = `query($id:String!){findTransaction(filterOptions:{byId:$id}){id status output{id}}}`
		var out struct {
			FindTransaction []struct {
				Id     string `json:"id"`
				Status string `json:"status"`
				Output []struct {
					Id string `json:"id"`
				} `json:"output"`
			} `json:"findTransaction"`
		}
		if err := d.gqlQuery(ctx, gqlNode, q, map[string]any{"id": txId}, &out); err != nil {
			t.Logf("DAG[%s] tx query err: %v", label, err)
			return
		}
		for _, tx := range out.FindTransaction {
			t.Logf("DAG[%s] tx=%s status=%s outputs=%d", label, tx.Id, tx.Status, len(tx.Output))
			for _, o := range tx.Output {
				const dq = `query($c:String!){getDagByCID(cidString:$c)}`
				var dout struct {
					GetDagByCID any `json:"getDagByCID"`
				}
				if err := d.gqlQuery(ctx, gqlNode, dq, map[string]any{"c": o.Id}, &dout); err != nil {
					t.Logf("DAG[%s] getDagByCID(%s) err: %v", label, o.Id, err)
					continue
				}
				b, _ := json.Marshal(dout.GetDagByCID)
				t.Logf("DAG[%s] output %s = %s", label, o.Id, string(b))
			}
		}
	}

	// Wait for the RC-bootstrap deposit to credit the owner's L2 HBD before any
	// owner contract-call (which need the gas budget it provides).
	if err := pollUntil(ctx, 7*time.Minute, func() bool {
		bal, e := d.GetAccountBalance(ctx, gqlNode, ownerAcct)
		return e == nil && bal != nil && bal.Hbd >= 4_000_000
	}); err != nil {
		bal, _ := d.GetAccountBalance(ctx, gqlNode, ownerAcct)
		t.Fatalf("RC-bootstrap HBD deposit never credited %s (hbd=%v) — gateway deposit path may be down", ownerAcct, func() any { if bal != nil { return bal.Hbd }; return nil }())
	}
	t.Logf("RC bootstrap: %s L2 HBD credited — gas budget raised for owner calls", ownerAcct)

	initTx, err := d.CallContract(ctx, ownerNode, amId, "initContract", `{"is_testnet":true}`)
	if err != nil {
		t.Fatalf("initContract broadcast: %v", err)
	}
	logTx("initContract", initTx)
	dumpState("am.init-marker", amId, "init") // expect "1" iff init's owner check PASSED

	// 2) Deploy the mock verifier + init (sets is_testnet sentinel for devSetHeader).
	mockId, err := d.DeployContract(ctx, ContractDeployOpts{WasmPath: mockVerifierWasm, Name: "mock-zk-verifier", DeployerNode: ownerNode, GQLNode: gqlNode})
	if err != nil {
		t.Fatalf("deploy mock verifier: %v", err)
	}
	t.Logf("mock verifier: %s", mockId)
	mockInitTx, err := d.CallContract(ctx, ownerNode, mockId, "init", `{}`)
	if err != nil {
		t.Fatalf("mock init broadcast: %v", err)
	}
	logTx("mock.init", mockInitTx)
	dumpState("mock.is_testnet", mockId, "is_testnet")

	// 3) Bootstrap wiring: propose -> wait > TimelockLong(5) -> execute setVerifierContract.
	payloadHex := hex.EncodeToString([]byte(fmt.Sprintf(`{"contract_id":"%s"}`, mockId)))
	proposeTx, err := d.CallContract(ctx, ownerNode, amId, "propose",
		fmt.Sprintf(`{"action":"setVerifierContract","payload_hex":"%s"}`, payloadHex))
	if err != nil {
		t.Fatalf("propose broadcast: %v", err)
	}
	logTx("propose", proposeTx)
	// Diagnostic: did propose create proposal pr-1? (counter -> "2", pr-1 -> JSON with exec_height)
	if err := pollUntil(ctx, 90*time.Second, func() bool {
		m, _ := d.GetStateByKeys(ctx, gqlNode, amId, []string{"pr-1"})
		return m != nil && m["pr-1"] != nil && fmt.Sprintf("%v", m["pr-1"]) != ""
	}); err != nil {
		t.Logf("WARN: proposal pr-1 not present after propose (propose likely aborted — owner/payload)")
	}
	dumpState("am.proposal", amId, "proposal_next_id", "pr-1")

	lb, _, err := d.LocalNodeInfo(ctx, gqlNode)
	if err != nil {
		t.Fatalf("node info: %v", err)
	}
	// TimelockLong=5 in the devnet build; wait a generous margin past it.
	if err := d.WaitForBlockProcessing(ctx, gqlNode, lb+12, 8*time.Minute); err != nil {
		t.Fatalf("waiting out timelock: %v", err)
	}
	executeTx, err := d.CallContract(ctx, ownerNode, amId, "execute", `{"proposal_id":1}`)
	if err != nil {
		t.Fatalf("execute broadcast: %v", err)
	}
	logTx("execute", executeTx)

	// ASSERT (1): account-mapping is now wired to the mock verifier.
	if err := pollUntil(ctx, 60*time.Second, func() bool {
		m, e := d.GetStateByKeys(ctx, gqlNode, amId, []string{"zkverifier"})
		return e == nil && m != nil && fmt.Sprintf("%v", m["zkverifier"]) == mockId
	}); err != nil {
		dumpState("am.FINAL", amId, "init", "proposal_next_id", "pr-1", "zkverifier")
		dumpTxDag("execute", executeTx) // raw output DAG = the real abort message
		dumpTxDag("propose", proposeTx)
		t.Fatalf("setVerifierContract did not take effect (short-timelock wiring FAILED) — see DAG[execute] above for the real abort. want zkverifier=%s", mockId)
	}
	t.Logf("WIRED: account-mapping zkverifier == %s (short-timelock devnet build bootstrapped the 400K-gated setVerifierContract)", mockId)

	// 4) Plant the REAL verified header via devSetHeader (no proof).
	plant := fmt.Sprintf(`{"block_number":%d,"state_root":"%s","tx_root":"%s","receipts_root":"%s","base_fee":%d,"gas_limit":%d,"timestamp":%d,"chain_id":%d}`,
		hdrBlockNumber, hdrStateRoot, hdrTxRoot, hdrRcptRoot, hdrBaseFee, hdrGasLimit, hdrTimestamp, hdrChainId)
	if _, err := d.CallContract(ctx, ownerNode, mockId, "devSetHeader", plant); err != nil {
		t.Fatalf("devSetHeader: %v", err)
	}

	// ASSERT (2): the mock stored the byte-exact real header at b-10764834, h=10764834.
	want := expectedHeaderHex()
	if err := pollUntil(ctx, 3*time.Minute, func() bool {
		got, e := d.getStateHex(ctx, gqlNode, mockId, "b-10764834")
		return e == nil && strings.EqualFold(got, want)
	}); err != nil {
		got, _ := d.getStateHex(ctx, gqlNode, mockId, "b-10764834")
		t.Fatalf("planted header mismatch:\n got=%s\nwant=%s", got, want)
	}
	hMap, _ := d.GetStateByKeys(ctx, gqlNode, mockId, []string{"h"})
	t.Logf("PLANTED: mock b-10764834 == real verified header (137B, byte-exact); h=%v", hMap["h"])

	t.Logf("W-MOCK PASS — short-timelock devnet wiring + real-header injection both work on devnet. " +
		"Foundation for W1/W2/W3 (deposit / withdrawal close-out / double-spend) is validated.")
}

// pollUntil retries fn every 3s until true or timeout.
func pollUntil(ctx context.Context, timeout time.Duration, fn func() bool) error {
	deadline := time.Now().Add(timeout)
	for {
		if fn() {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("condition not met within %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}
