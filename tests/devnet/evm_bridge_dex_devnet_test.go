//go:build evm_devnet

// DEX + EVM bridge end to end on devnet: the HIGH items of the DEX half of the
// EVM batch proven on a live network with the real pool, router, bridge and
// bot (11-03), not only on wasm:
//
//	D1 SP-0b     the router lists usdc from the bridge's getInfo({"symbol":"usdc"});
//	D2 SP-0      add_liquidity draws USDC from the bridge (the pool names the asset);
//	D3 SP-0c     a swap settled to Ethereum: the router withdraws through
//	             unmapERC20 and the bot pays it on L1; the router keeps nothing;
//	D4 refund_to a swap settled to an address the token refuses: the L1
//	             transfer reverts and the refund reaches the trader, not the router;
//	D5 C3        a withdrawal over the per-block cap is refused, one within it accepted.
//
// Needs DEX_WASM and DEX_ROUTER_WASM (dex-contracts bin/dex.wasm and
// bin/dex-router-v2.wasm) besides the EVM artifacts. It restarts every node
// to put the pool on the pendulum whitelist: run it alone, as the list runner
// does, on normal blocks (like WUSDC it registers no external hold for the
// bot, so fast blocks could move the test clock past the bot's rounds).
package devnet

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"vsc-node/lib/dids"

	ethcommon "github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

// callOutcome is a call's own result in the contract output that carries it
// (one output can carry several calls: matched by its index in inputs).
func (d *Devnet) callOutcome(ctx context.Context, node int, txID string, timeout time.Duration) (string, bool, error) {
	const q = `query($id:String!){findContractOutput(filterOptions:{byInput:$id}){inputs results{ret ok}}}`
	var ret string
	var ok, found bool
	err := pollUntil(ctx, timeout, func() bool {
		var out struct {
			FindContractOutput []struct {
				Inputs  []string `json:"inputs"`
				Results []struct {
					Ret string `json:"ret"`
					Ok  bool   `json:"ok"`
				} `json:"results"`
			} `json:"findContractOutput"`
		}
		if d.gqlQuery(ctx, node, q, map[string]any{"id": txID}, &out) != nil {
			return false
		}
		for _, o := range out.FindContractOutput {
			for i, in := range o.Inputs {
				if in == txID && i < len(o.Results) {
					ret, ok, found = o.Results[i].Ret, o.Results[i].Ok, true
				}
			}
		}
		return found
	})
	return ret, ok, err
}

func TestEVMBridge_DexUsdcThroughTheRouter(t *testing.T) {
	if testing.Short() {
		t.Skip("devnet")
	}
	dexWasm, routerWasm := os.Getenv("DEX_WASM"), os.Getenv("DEX_ROUTER_WASM")
	if dexWasm == "" || routerWasm == "" {
		t.Skip("DEX_WASM and DEX_ROUTER_WASM (dex-contracts bin/) required")
	}
	ctx, cancel := devnetWithTimeout(context.Background(), 150*time.Minute)
	t.Cleanup(cancel)
	e := setupEvmBridgeDevnet(t, ctx)
	d := e.d
	const trader = "hive:magi.test1" // node 1's account: deploys (owns) the pool and router, provides liquidity, trades
	eth := func(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(1e18)) }
	usd := func(n int64) *big.Int { return big.NewInt(n * 1_000_000) }

	// The pool and the router, on the pendulum whitelist (sysconfig + restart),
	// before the vault keygen and the bot.
	sharedDeployMu.Lock()
	poolID, err := deployWithRetry(ctx, d, ContractDeployOpts{WasmPath: dexWasm, Name: "dex-hbd-usdc", DeployerNode: 1, GQLNode: e.gqlNode}, 3)
	var routerID string
	if err == nil {
		routerID, err = deployWithRetry(ctx, d, ContractDeployOpts{WasmPath: routerWasm, Name: "dex-router", DeployerNode: 1, GQLNode: e.gqlNode}, 3)
	}
	sharedDeployMu.Unlock()
	if err != nil {
		t.Fatalf("deploy dex contracts: %v", err)
	}
	runDexPhases(t, ctx, e, poolID, routerID, trader, eth, usd)
}

func runDexPhases(t *testing.T, ctx context.Context, e *evmBridgeEnv, poolID, routerID, trader string, eth, usd func(int64) *big.Int) {
	d := e.d
	if err := d.setPendulumWhitelistAndRestart(ctx, []string{poolID}); err != nil {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("pendulum whitelist + restart: %v", err)
	}
	head := nodeBlock(t, d, ctx, 1)
	for n := 1; n <= d.cfg.Nodes; n++ {
		if err := d.WaitForBlockProcessing(ctx, n, head+10, 8*time.Minute); err != nil {
			dumpDiagnostics(t, d, ctx)
			t.Fatalf("magi-%d did not resume after the restart: %v", n, err)
		}
	}
	waitForFeedWarmup(t, d, ctx)

	// L1: USDC, registered on the bridge; the bot relays and watches it.
	anvil := StartAnvil(t, ctx, evmChainID, nextAnvilPort())
	t.Cleanup(anvil.Stop)
	ownerKey := mustKey(t)
	if err := anvil.SetBalance(ctx, cryptoPubkeyAddr(ownerKey), eth(100)); err != nil {
		t.Fatal(err)
	}
	usdc, err := anvil.DeployMockUSDC(ctx, ownerKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.proposeExecute("registerToken", fmt.Sprintf(`{"address":"%s","symbol":"usdc","decimals":6,"min_withdrawal":1000000}`, usdc.Hex())); err != nil {
		t.Fatalf("registerToken usdc: %v", err)
	}
	if err := pollUntil(ctx, evmL2Wait, func() bool { return e.stateStr("ts-usdc") != "" }); err != nil {
		t.Fatalf("usdc never registered on the bridge")
	}
	botKey := mustKey(t)
	botDID := dids.NewEthDID(ethCrypto.PubkeyToAddress(botKey.PublicKey).Hex()).String()
	if err := e.proposeExecute("registerRelayer", fmt.Sprintf(`{"account":"%s"}`, botDID)); err != nil {
		t.Fatalf("registerRelayer: %v", err)
	}
	if err := e.fundDID(botDID, "20000.000"); err != nil {
		t.Fatalf("fund bot DID: %v", err)
	}
	e.botAttests = true
	vault, err := e.tssVault(25 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := anvil.SetBalance(ctx, vault, eth(10)); err != nil {
		t.Fatal(err)
	}
	bot := StartBot(t, ctx, BotConfig{
		EthRPC: anvil.URL, ContractID: e.amID, GraphQLURL: d.GQLEndpoint(e.gqlNode),
		EthPrivHex: hex.EncodeToString(ethCrypto.FromECDSA(botKey)), Tokens: usdc.Hex(),
	})
	t.Cleanup(bot.Stop)
	botLogs := func() string { return strings.Join(bot.Logs(), "\n") }
	if !bot.WaitForLog(ctx, "A18: vault is the TSS key's address", evmL2Wait+10*time.Minute) {
		t.Fatalf("the bot never took the vault:\n%s", botLogs())
	}

	// Deposits: 6 ETH (seeds the gas reserve that pays token withdrawals' gas)
	// and 25 USDC from an Ethereum user, who sends 22 USDC to the trader on L2.
	depKey := mustKey(t)
	depAddr := ethCrypto.PubkeyToAddress(depKey.PublicKey)
	user := evmDID(depAddr)
	if err := anvil.SetBalance(ctx, depAddr, eth(7)); err != nil {
		t.Fatal(err)
	}
	_, ethBlock, err := anvil.SendETH(ctx, depKey, vault, eth(6))
	if err != nil {
		t.Fatalf("ETH deposit: %v", err)
	}
	if err := e.plantFinalized(anvil, ethBlock); err != nil {
		t.Fatal(err)
	}
	if err := pollUntil(ctx, 6*time.Minute, func() bool { return e.stateStr("gr") != "" }); err != nil {
		t.Fatalf("the ETH deposit never seeded the gas reserve\nbot:\n%s", botLogs())
	}
	if rc, err := anvil.SendTx(ctx, ownerKey, &usdc, usdcCall("mint(address,uint256)", depAddr, usd(30)), 100_000); err != nil || rc.Status != 1 {
		t.Fatalf("mint: %v", err)
	}
	rc, err := anvil.SendTx(ctx, depKey, &usdc, usdcCall("transfer(address,uint256)", vault, usd(25)), 100_000)
	if err != nil || rc.Status != 1 {
		t.Fatalf("USDC deposit: %v", err)
	}
	if err := e.plantFinalized(anvil, rc.BlockNumber.Uint64()); err != nil {
		t.Fatal(err)
	}
	if err := pollUntil(ctx, 6*time.Minute, func() bool { return e.stateStr("a-"+user+"-usdc") == "25000000" }); err != nil {
		t.Fatalf("the USDC deposit was never mapped\nbot:\n%s", botLogs())
	}
	if err := e.fundDID(user, "10000.000"); err != nil {
		t.Fatalf("fund user DID: %v", err)
	}
	K, err := NewEthKey(hex.EncodeToString(ethCrypto.FromECDSA(depKey)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.CallContractEvm(ctx, e.gqlNode, K, e.amID, "transfer", fmt.Sprintf(`{"to":"%s","amount":"22000000","asset":"usdc"}`, trader), 5_000_000); err != nil {
		t.Fatalf("transfer usdc to the trader: %v", err)
	}
	if err := pollUntil(ctx, evmL2Wait, func() bool { return e.stateStr("a-"+trader+"-usdc") == "22000000" }); err != nil {
		t.Fatalf("the trader never got usdc: %q", e.stateStr("a-"+trader+"-usdc"))
	}
	if _, err := d.Deposit(ctx, 1, "70.000", "hbd"); err != nil {
		t.Fatalf("deposit hbd: %v", err)
	}
	if !waitForL2Balance(t, d, ctx, 1, trader, "hbd", 65_000, 4*time.Minute) {
		t.Fatal("the trader's HBD deposit was never credited")
	}

	// D1 (SP-0b): pool and router initialized; the router lists usdc from the bridge.
	routerState := func(key string) string {
		m, _ := d.GetStateByKeys(ctx, e.gqlNode, routerID, []string{key})
		if v, ok := m[key]; ok && v != nil {
			return fmt.Sprintf("%v", v)
		}
		return ""
	}
	poolState := func(key string) string {
		m, _ := d.GetStateByKeys(ctx, e.gqlNode, poolID, []string{key})
		if v, ok := m[key]; ok && v != nil {
			return fmt.Sprintf("%v", v)
		}
		return ""
	}
	if _, err := d.CallContract(ctx, 1, poolID, "init", fmt.Sprintf(`{"asset0":"hbd","asset1":"usdc","asset1_mapping_contract":"%s","fee_bps":8,"router_contract":"%s"}`, e.amID, routerID)); err != nil {
		t.Fatalf("pool init: %v", err)
	}
	if _, err := d.CallContract(ctx, 1, routerID, "init", ""); err != nil {
		t.Fatalf("router init: %v", err)
	}
	if err := pollUntil(ctx, evmL2Wait, func() bool { return routerState("version") != "" && poolState("a1n") == "usdc" }); err != nil {
		t.Fatalf("pool/router init never applied (router version %q)", routerState("version"))
	}
	if _, err := d.CallContract(ctx, 1, routerID, "register_token", `{"name":"HBD","chain":"HIVE"}`); err != nil {
		t.Fatalf("register hbd: %v", err)
	}
	if _, err := d.CallContract(ctx, 1, routerID, "register_token", fmt.Sprintf(`{"name":"usdc","chain":"ETH","mapping_contract":"%s"}`, e.amID)); err != nil {
		t.Fatalf("register usdc: %v", err)
	}
	if err := pollUntil(ctx, evmL2Wait, func() bool { return routerState("asset-usdc") != "" }); err != nil {
		t.Fatalf("D1: the router never listed usdc from the bridge's getInfo (SP-0b)")
	}
	if _, err := d.CallContract(ctx, 1, routerID, "register_pool", fmt.Sprintf(`{"asset0":"hbd","asset1":"usdc","dex_contract_id":"%s"}`, poolID)); err != nil {
		t.Fatalf("register pool: %v", err)
	}
	if err := pollUntil(ctx, evmL2Wait, func() bool { return routerState("pool-hbd-usdc") != "" }); err != nil {
		t.Fatal("register_pool never applied")
	}
	t.Logf("D1 PASS (SP-0b): the router listed usdc from the bridge (asset-usdc %q)", routerState("asset-usdc"))

	// D2 (SP-0): 20 HBD + 20 USDC of liquidity; the pool draws the USDC from the bridge.
	if _, err := d.CallContract(ctx, 1, e.amID, "approve", fmt.Sprintf(`{"spender":"contract:%s","amount":"20000000","asset":"usdc"}`, poolID)); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := d.CallContractWithIntents(ctx, 1, poolID, "add_liquidity", fmt.Sprintf(`{"amount0":"20000","amount1":"20000000","recipient":"%s"}`, trader),
		[]map[string]interface{}{{"type": "transfer.allow", "args": map[string]string{"token": "hbd", "limit": "20.000"}}}, 20000); err != nil {
		t.Fatalf("add_liquidity: %v", err)
	}
	if err := pollUntil(ctx, evmL2Wait, func() bool { return poolState("r1") == "20000000" }); err != nil {
		t.Fatalf("D2: the pool never drew 20 USDC from the bridge (SP-0): r0=%q r1=%q", poolState("r0"), poolState("r1"))
	}
	if got := e.stateStr("a-contract:" + poolID + "-usdc"); got != "20000000" {
		t.Fatalf("D2: the pool's USDC on the bridge %q, want 20000000", got)
	}
	t.Logf("D2 PASS (SP-0): the pool drew 20 USDC from the bridge; reserves %s HBD / %s USDC", poolState("r0"), poolState("r1"))

	// D3 (SP-0c): 5 HBD -> USDC settled to an Ethereum address, paid by the bot.
	swap := func(to string, milliHBD int64) {
		t.Helper()
		instr := fmt.Sprintf(`{"type":"swap","version":"1.0.0","asset_in":"hbd","asset_out":"usdc","amount_in":"%d","recipient":"%s","destination_chain":"ETH","max_fee":3000000000000000}`, milliHBD, to)
		limit := fmt.Sprintf("%d.%03d", milliHBD/1000, milliHBD%1000)
		if _, err := d.CallContractWithIntents(ctx, 1, routerID, "execute", instr,
			[]map[string]interface{}{{"type": "transfer.allow", "args": map[string]string{"token": "hbd", "limit": limit}}}, 30000); err != nil {
			t.Fatalf("router execute: %v", err)
		}
	}
	// sent waits for the vault's L1 tx at nonce, then plants its block so the
	// bot can confirm it (as WUSDC does).
	sent := func(nonce, from uint64) *ethtypes.Receipt {
		t.Helper()
		var tx *ethtypes.Transaction
		if err := pollUntil(ctx, 25*time.Minute, func() bool {
			got, _, found, ferr := anvil.FindTxFrom(ctx, vault, nonce, from)
			tx = got
			return ferr == nil && found
		}); err != nil {
			t.Fatalf("the vault never sent nonce %d on L1\nbot:\n%s", nonce, botLogs())
		}
		rc, err := anvil.cli.TransactionReceipt(ctx, tx.Hash())
		if err != nil {
			t.Fatal(err)
		}
		if err := e.plantFinalized(anvil, rc.BlockNumber.Uint64()); err != nil {
			t.Fatal(err)
		}
		return rc
	}
	l1To := ethcommon.HexToAddress("0x00000000000000000000000000000000000d3e01")
	swap(l1To.Hex(), 5000)
	rc0 := sent(0, ethBlock)
	paid, _ := anvil.ERC20BalanceOf(ctx, usdc, l1To)
	if rc0.Status != 1 || paid == nil || paid.Sign() <= 0 {
		t.Fatalf("D3: the routed withdrawal status=%d, %v USDC reached %s (SP-0c)\nbot:\n%s", rc0.Status, paid, l1To.Hex(), botLogs())
	}
	if got := e.stateStr("a-contract:" + routerID + "-usdc"); got != "" && got != "0" {
		t.Fatalf("D3: the router kept %s usdc", got)
	}
	t.Logf("D3 PASS (SP-0c): 5 HBD swapped and settled to Ethereum: %s USDC units paid to %s by the bot; the router holds none", paid, l1To.Hex())

	// D4 (refund_to): 3 HBD -> USDC settled to the token's own address, which
	// MockUSDC refuses (Circle's rule): the L1 transfer reverts and the refund
	// is the trader's.
	before := e.stateStr("a-" + trader + "-usdc")
	swap(usdc.Hex(), 3000)
	if rc1 := sent(1, rc0.BlockNumber.Uint64()); rc1.Status != 0 {
		t.Fatalf("D4: the withdrawal to the token's own address did not revert on L1 (status %d)", rc1.Status)
	}
	if err := pollUntil(ctx, 30*time.Minute, func() bool { return e.stateStr("a-"+trader+"-usdc") != before }); err != nil {
		t.Fatalf("D4: the reverted routed withdrawal was never refunded to the trader (refund_to): %q\nbot:\n%s", e.stateStr("a-"+trader+"-usdc"), botLogs())
	}
	if got := e.stateStr("a-contract:" + routerID + "-usdc"); got != "" && got != "0" {
		t.Fatalf("D4: the refund went to the router (%s usdc), not the trader", got)
	}
	t.Logf("D4 PASS (refund_to): the reverted routed withdrawal refunded the trader (%s -> %s usdc); the router holds none", before, e.stateStr("a-"+trader+"-usdc"))

	// D5 (C3): a 2 USDC per-block cap; 3 USDC refused, 1 USDC accepted.
	if err := e.proposeExecute("setMaxWithdrawPerBlock", `{"asset":"usdc","amount":"2000000"}`); err != nil {
		t.Fatalf("setMaxWithdrawPerBlock: %v", err)
	}
	if err := pollUntil(ctx, 5*time.Minute, func() bool { return e.stateStr("mwb-usdc") == "2000000" }); err != nil {
		t.Fatalf("D5: the cap was never set: %q", e.stateStr("mwb-usdc"))
	}
	userBal := e.stateStr("a-" + user + "-usdc")
	unmap := func(amount string) (string, bool) {
		t.Helper()
		id, err := d.CallContractEvm(ctx, e.gqlNode, K, e.amID, "unmapERC20", fmt.Sprintf(`{"to":"%s","amount":"%s","token_address":"%s"}`, depAddr.Hex(), amount, usdc.Hex()), 5_000_000)
		if err != nil {
			t.Fatalf("unmapERC20 submit: %v", err)
		}
		ret, ok, err := d.callOutcome(ctx, e.gqlNode, id, 5*time.Minute)
		if err != nil {
			t.Fatalf("unmapERC20 %s: no output: %v", amount, err)
		}
		return ret, ok
	}
	if ret, ok := unmap("3000000"); ok || !strings.Contains(ret, "rate limit") {
		t.Fatalf("D5: 3 USDC over a 2 USDC cap: ok=%v %q", ok, ret)
	} else if got := e.stateStr("a-" + user + "-usdc"); got != userBal {
		t.Fatalf("D5: the refused withdrawal debited the user (%s -> %s)", userBal, got)
	} else {
		t.Logf("D5: 3 USDC refused: %s", ret)
	}
	if ret, ok := unmap("1000000"); !ok {
		t.Fatalf("D5: 1 USDC within the cap refused: %q", ret)
	}
	t.Logf("D5 PASS (C3): a withdrawal over the per-block cap refused, one within it accepted")
}
