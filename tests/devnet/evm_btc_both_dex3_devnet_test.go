//go:build evm_devnet

// DEX3 (owner 11-11: "since theres usdc eth and btc pool now ... a million
// things can go to shit"): the DEX with three pools, BTC/HBD, ETH/HBD and
// HBD/USDC, on the router, over the BOTH devnet, while both vaults rotate.
// Swaps cross pools through HBD (two hops) and settle to the other chain: a
// BTC -> ETH and an ETH -> USDC swap paid out on Ethereum while the EVM batch
// is still sweeping (each payout is a withdrawal queued in the migration), and
// a USDC -> BTC swap paid out on Bitcoin once the BTC sweep is in. Every
// payout must arrive, the router must keep nothing, and each bridge's custody
// must still cover its books when both rotations are done.
//
// Needs DEX_WASM and DEX_ROUTER_WASM (dex-contracts bin/) besides the BOTH
// inputs. The pools go on the pendulum whitelist with one more restart of
// every node before the EVM side starts.
//
// Run: go test -tags evm_devnet -run TestBothBridges_DEX3 ./tests/devnet/ -timeout 330m -v
package devnet

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

type dex3 struct {
	b                          *bothEnv
	poolBTC, poolETH, poolUSDC string
	router                     string
}

const dex3Trader = "hive:magi.test1" // node 1's account: holds the BTC vfSetup mapped, provides liquidity, trades

// newBothEnvDEX3 is newBothEnv with the three pools and the router deployed and
// whitelisted before the EVM side starts.
func newBothEnvDEX3(t *testing.T, budget time.Duration) *dex3 {
	t.Helper()
	dexWasm, routerWasm := os.Getenv("DEX_WASM"), os.Getenv("DEX_ROUTER_WASM")
	if dexWasm == "" || routerWasm == "" {
		t.Skip("DEX_WASM and DEX_ROUTER_WASM (dex-contracts bin/) required")
	}
	ctx, e, v := bothDevnet(t, budget, true)
	e.d.includedPatience = 15 * time.Minute
	d := e.d
	x := &dex3{}
	var err error
	sharedDeployMu.Lock()
	for _, p := range []struct {
		name string
		id   *string
	}{{"dex-btc-hbd", &x.poolBTC}, {"dex-eth-hbd", &x.poolETH}, {"dex-hbd-usdc", &x.poolUSDC}} {
		if err == nil {
			*p.id, err = deployWithRetry(ctx, d, ContractDeployOpts{WasmPath: dexWasm, Name: p.name, DeployerNode: 1, GQLNode: e.gqlNode}, 3)
		}
	}
	if err == nil {
		x.router, err = deployWithRetry(ctx, d, ContractDeployOpts{WasmPath: routerWasm, Name: "dex-router", DeployerNode: 1, GQLNode: e.gqlNode}, 3)
	}
	sharedDeployMu.Unlock()
	if err != nil {
		t.Fatalf("deploy dex contracts: %v", err)
	}
	if err := d.setPendulumWhitelistAndRestart(ctx, []string{x.poolBTC, x.poolETH, x.poolUSDC}); err != nil {
		t.Fatalf("pendulum whitelist + restart: %v", err)
	}
	head := nodeBlock(t, d, ctx, 1)
	for n := 1; n <= d.cfg.Nodes; n++ {
		if err := d.WaitForBlockProcessing(ctx, n, head+10, 8*time.Minute); err != nil {
			t.Fatalf("magi-%d did not resume after the whitelist restart: %v", n, err)
		}
	}
	waitForFeedWarmup(t, d, ctx)
	b := &bothEnv{t: t, ctx: ctx, d: d, e: e, v: v, w: bbWeatherLevel("none"), timings: map[string]time.Duration{}}
	bctx, cancel := context.WithCancel(ctx)
	b.stop = cancel
	t.Cleanup(cancel)
	t.Logf("DEX3 devnet up: BTC vault %s, EVM bridge %s, pools btc %s eth %s usdc %s, router %s", v.cid, e.amID, x.poolBTC, x.poolETH, x.poolUSDC, x.router)
	b.startEVM(bctx)
	b.meter = startMeter(bctx, b)
	t.Cleanup(func() { b.meter.report(t) })
	x.b = b
	return x
}

// call runs action on contract from node 1 (the trader) and fails the test if
// the contract refused it.
func (x *dex3) call(contract, action, payload string, intents []map[string]interface{}, rc int) {
	t, b := x.b.t, x.b
	var id string
	var err error
	if intents == nil {
		id, err = b.d.CallContract(b.ctx, 1, contract, action, payload)
	} else {
		id, err = b.d.CallContractWithIntents(b.ctx, 1, contract, action, payload, intents, rc)
	}
	if err != nil {
		t.Fatalf("%s.%s: %v", contract, action, err)
	}
	if ok, msg := b.e.outResult(b.ctx, id); !ok {
		t.Fatalf("%s.%s refused: %s (payload %s)", contract, action, msg, payload)
	}
}

func hbdAllow(milli int64) []map[string]interface{} {
	return []map[string]interface{}{{"type": "transfer.allow", "args": map[string]string{"token": "hbd", "limit": fmt.Sprintf("%d.%03d", milli/1000, milli%1000)}}}
}

// setup funds the trader with ETH and USDC (through a second Ethereum
// depositor), HBD, and lists the three pools with liquidity.
func (x *dex3) setup() {
	b := x.b
	t, e, d, ctx := b.t, b.e, b.d, b.ctx
	// ETH and USDC for the trader: a second depositor maps 2 ETH and 30 USDC
	// and sends 1 ETH and 25 USDC to the trader on L2.
	depKey := mustKey(t)
	depAddr := ethCrypto.PubkeyToAddress(depKey.PublicKey)
	dep := evmDID(depAddr)
	if err := b.anvil.SetBalance(ctx, depAddr, bbEth(3)); err != nil {
		t.Fatal(err)
	}
	if rc, err := b.anvil.SendTx(ctx, b.usdcOwner, &b.usdc, usdcCall("mint(address,uint256)", depAddr, bbUSD(30)), 100_000); err != nil || rc.Status != 1 {
		t.Fatalf("mint: %v", err)
	}
	if _, _, err := b.anvil.SendETH(ctx, depKey, b.vault0, bbEth(2)); err != nil {
		t.Fatal(err)
	}
	if rc, err := b.anvil.SendTx(ctx, depKey, &b.usdc, usdcCall("transfer(address,uint256)", b.vault0, bbUSD(30)), 100_000); err != nil || rc.Status != 1 {
		t.Fatalf("usdc deposit: %v", err)
	}
	// 10,000 HBD as the other user DIDs: the transfers below declare rc_limit 5,000,000, and 1,000 HBD
	// gives only 1,000,000 RC (DEX3 11-11 16:14: "not enough RCS available: 1000000 < 5000000").
	if err := e.fundDID(dep, "10000.000"); err != nil {
		t.Fatal(err)
	}
	if err := b.botPoll(20*time.Minute, func() bool {
		return e.stateStr("a-"+dep+"-eth") != "" && strings.SplitN(e.stateStr("a-"+dep+"-usdc"), "|", 2)[0] == bbUSD(30).String()
	}); err != nil {
		t.Fatalf("the DEX depositor's ETH/USDC never mapped\nbot:\n%s", b.botLogs())
	}
	K, err := NewEthKey(hex.EncodeToString(ethCrypto.FromECDSA(depKey)))
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range []string{
		fmt.Sprintf(`{"to":"%s","amount":"%s","asset":"eth"}`, dex3Trader, bbEth(1)),
		fmt.Sprintf(`{"to":"%s","amount":"%s","asset":"usdc"}`, dex3Trader, bbUSD(25)),
	} {
		id, err := d.CallContractEvm(ctx, e.gqlNode, K, e.amID, "transfer", tr, 5_000_000)
		if err != nil {
			t.Fatalf("transfer to the trader: %v", err)
		}
		if ok, msg := e.outResult(ctx, id); !ok {
			t.Fatalf("transfer to the trader refused: %s", msg)
		}
	}
	if _, err := d.Deposit(ctx, 1, "300.000", "hbd"); err != nil {
		t.Fatalf("deposit hbd: %v", err)
	}
	if !waitForL2Balance(t, d, ctx, 1, dex3Trader, "hbd", 290_000, 5*time.Minute) {
		t.Fatal("the trader's HBD was never credited")
	}

	// Pools (assets in alphabetical order), router, tokens.
	x.call(x.poolBTC, "init", fmt.Sprintf(`{"asset0":"btc","asset1":"hbd","asset0_mapping_contract":"%s","fee_bps":8,"router_contract":"%s"}`, b.v.cid, x.router), nil, 0)
	x.call(x.poolETH, "init", fmt.Sprintf(`{"asset0":"eth","asset1":"hbd","asset0_mapping_contract":"%s","fee_bps":8,"router_contract":"%s"}`, e.amID, x.router), nil, 0)
	x.call(x.poolUSDC, "init", fmt.Sprintf(`{"asset0":"hbd","asset1":"usdc","asset1_mapping_contract":"%s","fee_bps":8,"router_contract":"%s"}`, e.amID, x.router), nil, 0)
	x.call(x.router, "init", "", nil, 0)
	x.call(x.router, "register_token", `{"name":"HBD","chain":"HIVE"}`, nil, 0)
	x.call(x.router, "register_token", fmt.Sprintf(`{"name":"btc","chain":"BTC","mapping_contract":"%s"}`, b.v.cid), nil, 0)
	x.call(x.router, "register_token", fmt.Sprintf(`{"name":"eth","chain":"ETH","mapping_contract":"%s"}`, e.amID), nil, 0)
	x.call(x.router, "register_token", fmt.Sprintf(`{"name":"usdc","chain":"ETH","mapping_contract":"%s"}`, e.amID), nil, 0)
	x.call(x.router, "register_pool", fmt.Sprintf(`{"asset0":"btc","asset1":"hbd","dex_contract_id":"%s"}`, x.poolBTC), nil, 0)
	x.call(x.router, "register_pool", fmt.Sprintf(`{"asset0":"eth","asset1":"hbd","dex_contract_id":"%s"}`, x.poolETH), nil, 0)
	x.call(x.router, "register_pool", fmt.Sprintf(`{"asset0":"hbd","asset1":"usdc","dex_contract_id":"%s"}`, x.poolUSDC), nil, 0)

	// Liquidity: 0.01 BTC, 0.5 ETH, 20 USDC, each against 50 HBD.
	x.call(b.v.cid, "approve", fmt.Sprintf(`{"spender":"contract:%s","amount":"1000000"}`, x.poolBTC), nil, 0)
	x.call(x.poolBTC, "add_liquidity", fmt.Sprintf(`{"amount0":"1000000","amount1":"50000","recipient":"%s"}`, dex3Trader), hbdAllow(50000), 20000)
	half := new(big.Int).Div(bbEth(1), big.NewInt(2))
	x.call(e.amID, "approve", fmt.Sprintf(`{"spender":"contract:%s","amount":"%s","asset":"eth"}`, x.poolETH, half), nil, 0)
	x.call(x.poolETH, "add_liquidity", fmt.Sprintf(`{"amount0":"%s","amount1":"50000","recipient":"%s"}`, half, dex3Trader), hbdAllow(50000), 20000)
	x.call(e.amID, "approve", fmt.Sprintf(`{"spender":"contract:%s","amount":"20000000","asset":"usdc"}`, x.poolUSDC), nil, 0)
	x.call(x.poolUSDC, "add_liquidity", fmt.Sprintf(`{"amount0":"50000","amount1":"20000000","recipient":"%s"}`, dex3Trader), hbdAllow(50000), 20000)
	// The router pulls a mapped input with the trader's allowance.
	x.call(b.v.cid, "approve", fmt.Sprintf(`{"spender":"contract:%s","amount":"1000000"}`, x.router), nil, 0)
	x.call(e.amID, "approve", fmt.Sprintf(`{"spender":"contract:%s","amount":"%s","asset":"eth"}`, x.router, bbEth(1)), nil, 0)
	x.call(e.amID, "approve", fmt.Sprintf(`{"spender":"contract:%s","amount":"5000000","asset":"usdc"}`, x.router), nil, 0)
	t.Logf("DEX3: three pools listed with liquidity; router allowances set")
}

// swap runs a router swap from the trader, settled to chain at to.
func (x *dex3) swap(in, out, amount, to, chain string, maxFee int64) {
	instr := fmt.Sprintf(`{"type":"swap","version":"1.0.0","asset_in":"%s","asset_out":"%s","amount_in":"%s","recipient":"%s","destination_chain":"%s","max_fee":%d}`,
		in, out, amount, to, chain, maxFee)
	x.call(x.router, "execute", instr, nil, 0)
	x.b.note("DEX3 swap %s -> %s (%s) settled to %s %s", in, out, amount, chain, to)
}

// btcCompletePending signs, broadcasts and relays the BTC spend that appeared
// after before (the harness plays the BTC bot, as btcWithdraw does).
func (x *dex3) btcCompletePending(before []string, keyId string) string {
	b := x.b
	t, d, ctx, cid := b.t, b.d, b.ctx, b.v.cid
	txid := ""
	for i := 0; i < 60 && txid == ""; i++ {
		devnetSleep(5 * time.Second)
		for _, id := range txSpendIds(t, d, ctx, cid) {
			if !contains(before, id) {
				txid = id
				break
			}
		}
	}
	if txid == "" {
		t.Fatal("no BTC spend appeared for the router's BTC settlement")
	}
	sd := waitSigningData(t, d, ctx, cid, txid)
	if sd == nil {
		t.Fatalf("no signing data for %s", txid)
	}
	var mtx wire.MsgTx
	if err := mtx.Deserialize(bytes.NewReader(sd.Tx)); err != nil {
		t.Fatal(err)
	}
	for _, uh := range sd.UnsignedSigHashes {
		sig := waitSignature(t, d, ctx, keyId, uh.SigHash)
		if sig == nil {
			t.Fatalf("%s did not sign input %d of %s", keyId, uh.Index, txid)
		}
		mtx.TxIn[uh.Index].Witness = wire.TxWitness{append(append([]byte{}, sig...), byte(txscript.SigHashAll)), []byte{0x01}, uh.WitnessScript}
	}
	var buf bytes.Buffer
	if err := mtx.BtcEncode(&buf, wire.ProtocolVersion, wire.WitnessEncoding); err != nil {
		t.Fatal(err)
	}
	bc, h, err := vfBroadcastAndMine(t, d, ctx, hex.EncodeToString(buf.Bytes()))
	if err != nil {
		t.Fatalf("broadcast the router's BTC settlement: %v", err)
	}
	return vfRelayAndConfirm(t, d, ctx, 1, cid, bc, h)
}

func TestBothBridges_DEX3_ThreePoolsThroughBothRotations(t *testing.T) {
	bbSkipShort(t)
	if !bbUSDC() {
		t.Skip("BB_USDC=0")
	}
	x := newBothEnvDEX3(t, 330*time.Minute)
	b := x.b
	x.setup()
	ethTo := ethCrypto.PubkeyToAddress(mustKey(t).PublicKey)
	usdcTo := ethCrypto.PubkeyToAddress(mustKey(t).PublicKey)
	var btcTo string
	b.runCycle(cycleHooks{
		usdcOwn: true,
		migrating: func() {
			// Both payouts are EVM withdrawals queued while the EVM batch sweeps.
			x.swap("btc", "eth", "100000", ethTo.Hex(), "ETH", 3_000_000_000_000_000)
			x.swap("eth", "usdc", new(big.Int).Div(bbEth(1), big.NewInt(100)).String(), usdcTo.Hex(), "ETH", 3_000_000_000_000_000)
		},
		btcSwept: func() {
			addr, err := b.d.bitcoinCli(b.ctx, "getnewaddress")
			if err != nil {
				t.Fatal(err)
			}
			btcTo = strings.TrimSpace(addr)
			before := txSpendIds(t, b.d, b.ctx, b.v.cid)
			x.swap("usdc", "btc", bbUSD(2).String(), btcTo, "BTC", 50_000)
			if st := x.btcCompletePending(before, b.btcKeyID(1)); st == "" {
				t.Fatalf("the router's BTC settlement never confirmed\n%s", strings.Join(b.notes, "\n"))
			}
			b.note("DEX3 USDC -> BTC settled on Bitcoin to %s", btcTo)
		},
		settled: func() {
			e := b.e
			if err := b.botPoll(60*time.Minute, func() bool { return b.l1Balance(ethTo).Sign() > 0 && b.usdcAt(usdcTo).Sign() > 0 }); err != nil {
				t.Fatalf("a DEX payout queued in the migration never arrived: ETH %s, USDC %s\nbot:\n%s", b.l1Balance(ethTo), b.usdcAt(usdcTo), b.botLogs())
			}
			got, err := b.d.bitcoinCli(b.ctx, "getreceivedbyaddress", btcTo, "0")
			if err != nil || strings.TrimSpace(got) == "0.00000000" {
				t.Fatalf("the USDC -> BTC payout never reached %s: %q %v", btcTo, got, err)
			}
			for _, k := range []string{"a-contract:" + x.router + "-eth", "a-contract:" + x.router + "-usdc"} {
				if v := e.stateStr(k); v != "" && v != "0" {
					t.Fatalf("the router kept %s = %s", k, v)
				}
			}
			if m, _ := b.d.GetStateByKeys(b.ctx, e.gqlNode, b.v.cid, []string{"a-contract:" + x.router}); m != nil {
				if v, ok := m["a-contract:"+x.router]; ok && v != nil && fmt.Sprint(v) != "" {
					if n := new(big.Int).SetBytes([]byte(fmt.Sprint(v))); n.Sign() != 0 {
						t.Fatalf("the router kept %s sats on the BTC contract", n)
					}
				}
			}
			vault1, _ := tssKeyAddress(e.keyPub(evmKeyName(1)))
			active := strings.SplitN(e.stateStr("s-usdc"), "|", 2)[0]
			if held := b.usdcAt(vault1); held.String() != active || b.usdcAt(b.vault0).Sign() != 0 {
				t.Fatalf("USDC custody after both rotations: gen1 holds %s, s-usdc active %s, gen0 %s", held, active, b.usdcAt(b.vault0))
			}
			t.Logf("CASE DEX3 PASS: three pools, BTC->ETH and ETH->USDC paid on Ethereum during the EVM migration (%s wei, %s USDC units), USDC->BTC paid on Bitcoin (%s BTC) after the BTC sweep; router empty; USDC custody %s at gen 1",
				b.l1Balance(ethTo), b.usdcAt(usdcTo), strings.TrimSpace(got), active)
		},
	})
}
