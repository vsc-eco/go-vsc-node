//go:build evm_realwasm

package main

// PAYOUT-1 end to end on the node's wasm runtime: the DEX router (return
// address on Hive), a USDC/HBD pool and the EVM bridge together. A swap paid
// from a Hive account and paid out in USDC on Ethereum is either issued at
// once, at the expected gas fee with nothing refunded later, or refused by the
// bridge, and then the router sends the HBD back to the account on Hive.
//
//	EVM_MAPPING_WASM=<bridge main.wasm> DEX_ROUTER_WASM=<dex-router-v2.wasm> DEX_POOL_WASM=<dex.wasm> \
//	  go test -tags evm_realwasm ./cmd/evm-mapping-bot/ -run RealWasm_PAYOUT1 -v

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/ethereum/go-ethereum/core/types"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"

	"math/big"
	"os"
	"strings"
	"testing"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/db/vsc/contracts"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	pendulumoracle "vsc-node/modules/incentive-pendulum/oracle"
	stateEngine "vsc-node/modules/state-processing"
)

const (
	poRouter = "vsc1Bpc3SgDqCRQxzeDrvV7T4XKV6BZuHmME5F"
	poPool   = "vsc1BmLNMQep1RaaUdYTPfEhqn1inESqNz4Ekt"
	poTrader = "hive:po-trader"
	poTo     = "0x00000000000000000000000000000000000000e2"
	// poHeadroom is HBD the trader keeps for RC; never swapped.
	poHeadroom = 1_000_000
)

func poCall(ct *test_utils.ContractTest, caller, contract, action, payload string, rc uint64, intents []contracts.Intent) test_utils.ContractTestCallResult {
	if intents == nil {
		intents = []contracts.Intent{}
	}
	return ct.Call(stateEngine.TxVscCallContract{
		Self: stateEngine.TxSelf{
			TxId: "potx", BlockId: "block:po", Index: 1, Timestamp: "2026-10-18T00:00:00",
			RequiredAuths: []string{caller}, RequiredPostingAuths: []string{},
		},
		ContractId: contract, Action: action, Payload: []byte(payload),
		RcLimit: rc, Intents: intents, Caller: caller,
	})
}

func poAllow(asset, limit string) []contracts.Intent {
	return []contracts.Intent{{Type: "transfer.allow", Args: map[string]string{"token": asset, "limit": limit}}}
}

// poWorld is rwUSDCBridge plus the router (registered with the bridge) and a
// USDC/HBD pool of 20/20, a 1,760 USDC/ETH gas rate and a 0.15 USDC proof fee.
func poWorld(t *testing.T) rwChain {
	t.Helper()
	c, _ := poWorldLP(t)
	return c
}

// poWorldLP is poWorld that also returns the LP account (5 USDC left on it).
func poWorldLP(t *testing.T) (rwChain, string) {
	t.Helper()
	router, pool := os.Getenv("DEX_ROUTER_WASM"), os.Getenv("DEX_POOL_WASM")
	if router == "" || pool == "" {
		t.Skip("DEX_ROUTER_WASM / DEX_POOL_WASM not set")
	}
	rw, err1 := os.ReadFile(router)
	pw, err2 := os.ReadFile(pool)
	if err1 != nil || err2 != nil {
		t.Fatal(err1, err2)
	}
	c, lp := rwUSDCBridge(t)
	ct := c.ct
	ct.RegisterContract(poRouter, "hive:owner", rw)
	ct.RegisterContract(poPool, "hive:owner", pw)
	ct.SetPendulumGeometry(pendulumoracle.GeometryOutputs{OK: true, V: 500_000, P: 250_000, E: 1_000_000, T: 1_000_000, SBps: 5000}, []string{poPool})
	ct.StateSet(rwBridge, "routerid", poRouter)
	ct.StateSet(rwBridge, "tgr-usdc", "1760000000")
	ct.StateSet(rwBridge, "pf-usdc", "150000")
	for _, step := range []struct{ contract, action, payload string }{
		{poRouter, "init", ""},
		{poRouter, "register_token", `{"name":"HBD","chain":"HIVE"}`},
		{poRouter, "register_token", `{"name":"usdc","chain":"ETH","mapping_contract":"` + rwBridge + `"}`},
		{poRouter, "register_pool", `{"asset0":"hbd","asset1":"usdc","dex_contract_id":"` + poPool + `"}`},
		{poPool, "init", `{"asset0":"usdc","asset1":"hbd","fee_bps":8,"asset0_mapping_contract":"` + rwBridge + `","router_contract":"` + poRouter + `"}`},
	} {
		if r := poCall(ct, "hive:owner", step.contract, step.action, step.payload, 20_000, nil); !r.Success {
			t.Fatalf("%s %s: %s: %s", step.contract, step.action, r.Err, r.ErrMsg)
		}
	}
	if r := rwCall(ct, lp, "approve", []byte(`{"spender":"contract:`+poPool+`","amount":"20000000","asset":"usdc"}`)); !r.Success {
		t.Fatalf("approve: %s: %s", r.Err, r.ErrMsg)
	}
	ct.Deposit(lp, 20_000, ledgerDb.AssetHbd)
	if r := poCall(ct, lp, poPool, "add_liquidity", `{"amount0":"20000","amount1":"20000000","recipient":"`+lp+`"}`, 20_000, poAllow("hbd", "20.000")); !r.Success {
		t.Fatalf("add_liquidity: %s: %s", r.Err, r.ErrMsg)
	}
	// 5 HBD to swap, plus HBD that backs the call's RC (the node keeps
	// rc_limit/1000 HBD back from an HBD input once free RC is spent).
	ct.Deposit(poTrader, 5_000+poHeadroom, ledgerDb.AssetHbd)
	rwHeaderAt(ct, 102, &fakeChain{}, rwBlockTime) // a fresh pricing header (base fee 1 gwei)
	return c, lp
}

func poSwap(returnToHive bool) string {
	ra := ""
	if returnToHive {
		ra = `,"return_address":{"chain":"HIVE","address":"` + poTrader + `"}`
	}
	return `{"type":"swap","version":"1.0.0","asset_in":"hbd","asset_out":"usdc","amount_in":"5000","min_amount_out":"1","recipient":"` + poTo + `","destination_chain":"ETH"` + ra + `}`
}

func poLogs(r test_utils.ContractTestCallResult, contract string) []string {
	return r.Logs[contract].Logs
}

func poHasLog(r test_utils.ContractTestCallResult, contract, prefix string) bool {
	for _, l := range poLogs(r, contract) {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	return false
}

// Paid out: issued in the same call at the expected gas fee (65,000 gas at
// 1.5 x 1 gwei + 2 gwei = 0.4004 USDC at 1,760 USDC/ETH), with no token gas
// refund on record, the Hive account as refund_to and HBD as return_asset.
func TestRealWasm_PAYOUT1_HivePaidUSDCPayoutIsIssuedAtTheExpectedFee(t *testing.T) {
	for _, returnToHive := range []bool{false, true} {
		c := poWorld(t)
		r := poCall(c.ct, poTrader, poRouter, "execute", poSwap(returnToHive), 60_000, poAllow("hbd", "5.000"))
		if !r.Success {
			t.Fatalf("return_to_hive=%v: %s: %s", returnToHive, r.Err, r.ErrMsg)
		}
		t.Logf("return_to_hive=%v: HBD -> USDC paid out on Ethereum: %d RC", returnToHive, r.RcUsed)
		ps := rwPending(t, c, 0)
		var raw map[string]any
		_ = json.Unmarshal([]byte(c.ct.StateGet(rwBridge, "d-0")), &raw)
		if ps.From != "contract:"+poRouter || raw["refund_to"] != poTrader || !strings.EqualFold(ps.To, poTo) {
			t.Fatalf("pending spend %s", c.ct.StateGet(rwBridge, "d-0"))
		}
		if got, _ := raw["return_asset"].(string); (got == "hbd") != returnToHive {
			t.Fatalf("return_to_hive=%v: return_asset %q", returnToHive, got)
		}
		if c.ct.StateGet(rwBridge, "gt-0") != "" {
			t.Fatal("a payout recorded a token gas refund share")
		}
		if !poHasLog(r, rwBridge, "tokengasfee|a=usdc|v=400400|") {
			t.Fatalf("bridge logs %v, want the 0.4004 USDC payout gas fee", poLogs(r, rwBridge))
		}
		if got := c.ct.GetBalance(poTrader, ledgerDb.AssetHbd); got != poHeadroom {
			t.Fatalf("trader HBD %d after the swap, want the headroom %d", got, poHeadroom)
		}
		if b := c.ct.StateGet(rwBridge, "a-contract:"+poRouter+"-usdc"); b != "" && b != "0" {
			t.Fatalf("the router kept %s USDC", b)
		}
	}
}

// A busy bridge (every withdrawal slot in flight, or the gas reserve short)
// does not bounce the swap: the payout waits in the bridge's queue and is sent
// at the expected fee once it can be (owner 11-21: wait and retry, THORChain's
// LackSigning). Nothing lands in the trader's Magi balance meanwhile.
func TestRealWasm_PAYOUT1_BusyBridgePayoutWaitsThenSends(t *testing.T) {
	for _, busy := range []string{"slots", "reserve"} {
		for _, returnToHive := range []bool{true, false} {
			c := poWorld(t)
			ct := c.ct
			gr := ct.StateGet(rwBridge, "gr")
			seth := ct.StateGet(rwBridge, "s-eth")
			switch busy {
			case "slots":
				ct.StateSet(rwBridge, "np", "8") // nonces 0..7 in flight (MaxInFlightWithdrawals)
			case "reserve":
				ct.StateSet(rwBridge, "gr", "1000")
				ct.StateSet(rwBridge, "s-eth", "1000|0|0|0")
			}
			r := poCall(ct, poTrader, poRouter, "execute", poSwap(returnToHive), 60_000, poAllow("hbd", "5.000"))
			if !r.Success {
				t.Fatalf("busy=%s return=%v: %s: %s", busy, returnToHive, r.Err, r.ErrMsg)
			}
			if poHasLog(r, poRouter, "swaprefund|") {
				t.Fatalf("busy=%s: the swap was sent back instead of waiting", busy)
			}
			if ct.StateGet(rwBridge, "wqt") != "1" {
				t.Fatalf("busy=%s: not queued (wqt %q)", busy, ct.StateGet(rwBridge, "wqt"))
			}
			if got := ct.GetBalance(poTrader, ledgerDb.AssetHbd); got != poHeadroom {
				t.Fatalf("trader Magi HBD %d", got)
			}
			if u := ct.StateGet(rwBridge, "a-"+poTrader+"-usdc"); u != "" && u != "0" {
				t.Fatalf("trader holds %s USDC on Magi", u)
			}
			// The bridge frees up: the drain sends it at the payout fee.
			switch busy {
			case "slots":
				ct.StateSet(rwBridge, "n", "8")
			case "reserve":
				ct.StateSet(rwBridge, "gr", gr)
				ct.StateSet(rwBridge, "s-eth", seth)
			}
			d := rwCallLimit(ct, rwRelayer, "drainWithdrawalQueue", []byte(`{"max":8}`), 60_000)
			if !d.Success {
				t.Fatalf("drain: %s: %s", d.Err, d.ErrMsg)
			}
			nonce := uint64(0)
			if busy == "slots" {
				nonce = 8
			}
			ps := rwPending(t, c, nonce)
			if !poHasLog(d, rwBridge, "tokengasfee|a=usdc|v=400400|") || ps.Asset != "usdc" {
				t.Fatalf("busy=%s: drained %s; logs %v", busy, c.ct.StateGet(rwBridge, "d-"+jsonNum(nonce)), poLogs(d, rwBridge))
			}
			t.Logf("busy=%s return=%v: queued (%d RC), sent by the drain (%d RC)", busy, returnToHive, r.RcUsed, d.RcUsed)
		}
	}
}

// A Hive-paid USDC payout that reverts on Ethereum is swapped back to HBD and
// sent to the trader on Hive, at no less than the swap's own rate less 5%
// (return_min). With the pool sandwiched around the (permissionless) revert
// confirmation the floor is not met: the trader is credited the refund in full
// and the sandwich gains nothing. Adapted from the 11-21 scrutiny (F1), which
// showed -39% without a floor.
func TestRealWasm_PAYOUT1_RevertedPayoutSwapsBackWithAFloor(t *testing.T) {
	for _, mode := range []string{"clean", "sandwich", "release", "release-eth"} {
		sandwich := mode != "clean"
		c, lp := poWorldLP(t)
		ct := c.ct
		r := poCall(ct, poTrader, poRouter, "execute", poSwap(true), 60_000, poAllow("hbd", "5.000"))
		if !r.Success {
			t.Fatalf("swap: %s: %s", r.Err, r.ErrMsg)
		}
		ps := rwPending(t, c, 0)
		var raw map[string]any
		_ = json.Unmarshal([]byte(ct.StateGet(rwBridge, "d-0")), &raw)
		floor, _ := new(big.Int).SetString(fmt.Sprint(raw["return_min"]), 10)
		if floor == nil || floor.Sign() <= 0 {
			t.Fatalf("pending spend has no return_min: %s", ct.StateGet(rwBridge, "d-0"))
		}
		chain, tx := rwMine(t, c, ps, 103, types.ReceiptStatusFailed)
		payload, err := buildConfirmSpendPayload(context.Background(), chain.rpc(t), tx.Hash().Hex(), 103, 0, ps)
		if err != nil {
			t.Fatal(err)
		}
		attacker := lp
		usdc0, _ := strconv.ParseInt(ct.StateGet(rwBridge, "a-"+attacker+"-usdc"), 10, 64)
		hbd0 := ct.GetBalance(attacker, ledgerDb.AssetHbd)
		if sandwich {
			if r := rwCall(ct, attacker, "approve", []byte(`{"spender":"contract:`+poRouter+`","amount":"100000000","asset":"usdc"}`)); !r.Success {
				t.Fatalf("approve: %s: %s", r.Err, r.ErrMsg)
			}
			poolUsdc, _ := strconv.ParseInt(ct.StateGet(rwBridge, "a-contract:"+poPool+"-usdc"), 10, 64)
			sell := min(poolUsdc/2, usdc0)
			if fr := poCall(ct, attacker, poRouter, "execute", fmt.Sprintf(`{"type":"swap","version":"1.0.0","asset_in":"usdc","asset_out":"hbd","amount_in":"%d","min_amount_out":"1","recipient":"%s"}`, sell, attacker), 60_000, nil); !fr.Success {
				t.Fatalf("front-run: %s: %s", fr.Err, fr.ErrMsg)
			}
		}
		poolHBD := ct.GetBalance("contract:"+poPool, ledgerDb.AssetHbd)
		cs := rwCall(ct, attacker, "confirmSpend", payload) // permissionless: anyone may submit it
		if !cs.Success {
			t.Fatalf("confirmSpend: %s: %s", cs.Err, cs.ErrMsg)
		}
		t.Logf("sandwich=%v: confirmSpend %d RC; bridge logs %v", sandwich, cs.RcUsed, poLogs(cs, rwBridge))
		back := poolHBD - ct.GetBalance("contract:"+poPool, ledgerDb.AssetHbd)
		// Nothing swaps inside the confirmSpend anyone can submit (and wrap):
		// the value is held for the trader, for the bridge's bot to swap back.
		if !poHasLog(cs, rwBridge, "payoutreturnheld|id=0|n=0|t="+poTrader+"|a=usdc|") || back != 0 {
			t.Fatalf("swapped inside confirmSpend (%d mHBD out of the pool) or not held: %v", back, poLogs(cs, rwBridge))
		}
		if ar := rwCallLimit(ct, attacker, "retryPayoutReturn", []byte(`{"id":0,"min_out":"1"}`), 60_000); ar.Success {
			t.Fatal("someone other than the bridge's bot ran the swap back")
		}
		if !sandwich {
			rr := poBotRetry(ct, 0)
			back = poolHBD - ct.GetBalance("contract:"+poPool, ledgerDb.AssetHbd)
			if !rr.Success || !poHasLog(rr, rwBridge, "payoutreturn|id=0|t="+poTrader+"|a=usdc|") {
				t.Fatalf("bot's swap back: %s: %s; logs %v", rr.Err, rr.ErrMsg, poLogs(rr, rwBridge))
			}
			if back < floor.Int64() {
				t.Fatalf("swapped back %d mHBD, below the floor %s", back, floor)
			}
			t.Logf("clean: held, then the bot swapped back %d mHBD (floor %s), output %s, %d RC", back, floor, rr.Ret, rr.RcUsed)
			if got := ct.GetBalance(poTrader, ledgerDb.AssetHbd); got != poHeadroom {
				t.Fatalf("trader Magi HBD %d: the HBD went to Hive, not the Magi balance", got)
			}
			if u := ct.StateGet(rwBridge, "a-"+poTrader+"-usdc"); u != "" && u != "0" {
				t.Fatalf("trader holds %s USDC on Magi", u)
			}
			continue
		}
		// Sandwiched: held for the trader (owner 11-21: to the sender or the
		// recipient, never a Magi balance); the pool is untouched.
		hold := "a-system:payout-return-usdc"
		if u := ct.StateGet(rwBridge, "a-"+poTrader+"-usdc"); u != "" && u != "0" {
			t.Fatalf("trader holds %s USDC on Magi", u)
		}
		if got := ct.StateGet(rwBridge, hold); got != ps.Amount.String() {
			t.Fatalf("held %q USDC, want the whole refund %s", got, ps.Amount)
		}
		if mode == "release-eth" {
			// The exit that needs no pool: the trader has the held USDC itself
			// sent to an Ethereum address of their choosing, as a payout.
			rel := poCall(ct, poTrader, rwBridge, "releasePayoutReturn", `{"id":0,"to":"`+poTo+`"}`, 30_000, nil)
			if !rel.Success {
				t.Fatalf("release to Ethereum: %s: %s", rel.Err, rel.ErrMsg)
			}
			var out *PendingSpend
			for n := uint64(0); n < 8 && out == nil; n++ {
				if v := ct.StateGet(rwBridge, "d-"+jsonNum(n)); strings.Contains(v, "system:payout-return") {
					out = rwPending(t, c, n)
				}
			}
			if out == nil || !strings.EqualFold(out.To, poTo) || out.Asset != "usdc" {
				t.Fatalf("no payout from the hold to %s: %v", poTo, poLogs(rel, rwBridge))
			}
			if h := ct.StateGet(rwBridge, hold); h != "0" || ct.StateGet(rwBridge, "phr-0") != "" {
				t.Fatalf("hold %q record %q after the release", h, ct.StateGet(rwBridge, "phr-0"))
			}
			if u := ct.StateGet(rwBridge, "a-"+poTrader+"-usdc"); u != "" && u != "0" {
				t.Fatalf("trader holds %s USDC on Magi", u)
			}
			t.Logf("released to Ethereum: %s USDC units to %s, %d RC", out.Amount, out.To, rel.RcUsed)
			continue
		}
		if mode == "release" {
			// The trader takes it back now, at a price of their own, while the
			// pool is still pushed.
			pool0 := ct.GetBalance("contract:"+poPool, ledgerDb.AssetHbd)
			rel := poCall(ct, poTrader, rwBridge, "releasePayoutReturn", `{"id":0,"min_out":"1"}`, 30_000, nil)
			if !rel.Success {
				t.Fatalf("release: %s: %s", rel.Err, rel.ErrMsg)
			}
			if other := poCall(ct, attacker, rwBridge, "releasePayoutReturn", `{"id":0,"min_out":"1"}`, 30_000, nil); other.Success {
				t.Fatal("someone else released the trader's return")
			}
			got := pool0 - ct.GetBalance("contract:"+poPool, ledgerDb.AssetHbd)
			if got <= 0 || ct.GetBalance(poTrader, ledgerDb.AssetHbd) != poHeadroom || ct.StateGet(rwBridge, hold) != "0" {
				t.Fatalf("release sent %d mHBD; trader Magi HBD %d; hold %q", got, ct.GetBalance(poTrader, ledgerDb.AssetHbd), ct.StateGet(rwBridge, hold))
			}
			t.Logf("released by the trader at its own price: %d mHBD to Hive (floor was %s), %d RC", got, floor, rel.RcUsed)
			continue
		}
		// The bot's swap back while the pool is pushed: the floor holds (the
		// first day), it fails and moves nothing.
		if rr := poBotRetry(ct, 0); rr.Success {
			t.Fatalf("retry ran below the floor: %v", poLogs(rr, rwBridge))
		}
		if ct.StateGet(rwBridge, hold) != ps.Amount.String() {
			t.Fatal("a refused retry moved the hold")
		}
		// Back-run: the attacker sells what it bought; it ends with less USDC.
		gain := ct.GetBalance(attacker, ledgerDb.AssetHbd) - hbd0
		if br := poCall(ct, attacker, poRouter, "execute", fmt.Sprintf(`{"type":"swap","version":"1.0.0","asset_in":"hbd","asset_out":"usdc","amount_in":"%d","min_amount_out":"1","recipient":"%s"}`, gain, attacker), 60_000, poAllow("hbd", fmt.Sprintf("%d.%03d", gain/1000, gain%1000))); !br.Success {
			t.Fatalf("back-run: %s: %s", br.Err, br.ErrMsg)
		}
		usdc1, _ := strconv.ParseInt(ct.StateGet(rwBridge, "a-"+attacker+"-usdc"), 10, 64)
		if usdc1 >= usdc0 {
			t.Fatalf("the sandwich profited: attacker USDC %d -> %d", usdc0, usdc1)
		}
		t.Logf("sandwich P&L: %+d USDC units", usdc1-usdc0)
		// The price is back: the keeper's retry sends it to the trader on Hive.
		pool1 := ct.GetBalance("contract:"+poPool, ledgerDb.AssetHbd)
		rr := poBotRetry(ct, 0)
		if !rr.Success || !poHasLog(rr, rwBridge, "payoutreturn|id=0|t="+poTrader+"|a=usdc|") {
			t.Fatalf("retry after the price returned: %s: %s; logs %v", rr.Err, rr.ErrMsg, poLogs(rr, rwBridge))
		}
		got := pool1 - ct.GetBalance("contract:"+poPool, ledgerDb.AssetHbd)
		if got < floor.Int64() || ct.GetBalance(poTrader, ledgerDb.AssetHbd) != poHeadroom || ct.StateGet(rwBridge, hold) != "0" {
			t.Fatalf("retry sent %d mHBD (floor %s); trader Magi HBD %d; hold %q", got, floor, ct.GetBalance(poTrader, ledgerDb.AssetHbd), ct.StateGet(rwBridge, hold))
		}
		t.Logf("held, then retried by the keeper: %d mHBD to Hive (floor %s), retry %d RC", got, floor, rr.RcUsed)
	}
}

// An ETH payout of a Hive-paid swap (HBD -> ETH, egl 60,000): it pays its gas
// limit at the expected price (60,000 x 3.5 gwei), the reserve fronts the rest
// and the books balance (Active - User == reserve) after the payout and after
// its confirmation; reverted, the ETH is swapped back to HBD on Hive above its
// floor. Adapted from the 11-21 scrutiny (F13).
func TestRealWasm_PAYOUT1_ETHPayoutAndItsRevert(t *testing.T) {
	for _, revert := range []bool{false, true} {
		c, lp := poETHWorld(t)
		ct := c.ct
		_ = lp
		books := func(label string) {
			t.Helper()
			parts := strings.Split(ct.StateGet(rwBridge, "s-eth"), "|")
			active, _ := new(big.Int).SetString(parts[0], 10)
			user, _ := new(big.Int).SetString(parts[1], 10)
			gr, _ := new(big.Int).SetString(ct.StateGet(rwBridge, "gr"), 10)
			if new(big.Int).Sub(active, user).Cmp(gr) != 0 {
				t.Fatalf("%s: Active-User %s != reserve %s", label, new(big.Int).Sub(active, user), gr)
			}
		}
		swap := `{"type":"swap","version":"1.0.0","asset_in":"hbd","asset_out":"eth","amount_in":"50000","min_amount_out":"1","recipient":"` + poTo + `","destination_chain":"ETH","return_address":{"chain":"HIVE","address":"` + poTrader + `"}}`
		r := poCall(ct, poTrader, poRouter, "execute", swap, 60_000, poAllow("hbd", "50.000"))
		if !r.Success {
			t.Fatalf("swap: %s: %s", r.Err, r.ErrMsg)
		}
		ps := rwPending(t, c, 0)
		fee := new(big.Int).Mul(big.NewInt(60_000), big.NewInt(3_500_000_000))
		var raw map[string]any
		_ = json.Unmarshal([]byte(ct.StateGet(rwBridge, "d-0")), &raw)
		found := false
		for _, l := range poLogs(r, rwBridge) {
			if strings.HasPrefix(l, "unmap|n=0|") && strings.Contains(l, "|g="+fee.String()+"|gs=user") {
				found = true
			}
		}
		if !found {
			t.Fatalf("unmap log does not show the 210,000 gwei payout fee: %v", poLogs(r, rwBridge))
		}
		books("after the payout")
		status := uint64(types.ReceiptStatusSuccessful)
		if revert {
			status = types.ReceiptStatusFailed
		}
		chain, tx := rwMine(t, c, ps, 103, status)
		payload, err := buildConfirmSpendPayload(context.Background(), chain.rpc(t), tx.Hash().Hex(), 103, 0, ps)
		if err != nil {
			t.Fatal(err)
		}
		poolHBD := ct.GetBalance("contract:"+poPool, ledgerDb.AssetHbd)
		cs := rwCall(ct, rwRelayer, "confirmSpend", payload)
		if !cs.Success {
			t.Fatalf("confirmSpend: %s: %s", cs.Err, cs.ErrMsg)
		}
		books("after the confirmation")
		if e := ct.StateGet(rwBridge, "a-"+poTrader+"-eth"); e != "" && e != "0" {
			t.Fatalf("revert=%v: trader holds %s wei on Magi", revert, e)
		}
		back := poolHBD - ct.GetBalance("contract:"+poPool, ledgerDb.AssetHbd)
		if revert {
			floor, _ := strconv.ParseInt(fmt.Sprint(raw["return_min"]), 10, 64)
			if !poHasLog(cs, rwBridge, "payoutreturnheld|id=0|n=0|") || back != 0 || floor <= 0 {
				t.Fatalf("reverted ETH payout: want it held (nothing swapped in confirmSpend), %d mHBD moved; logs %v", back, poLogs(cs, rwBridge))
			}
			rr := poBotRetry(ct, 0)
			back = poolHBD - ct.GetBalance("contract:"+poPool, ledgerDb.AssetHbd)
			if !rr.Success || !poHasLog(rr, rwBridge, "payoutreturn|id=0|") || back < floor {
				t.Fatalf("bot's swap back: %s: %s, %d mHBD back (floor %d); logs %v", rr.Err, rr.ErrMsg, back, floor, poLogs(rr, rwBridge))
			}
			books("after the swap back")
			t.Logf("reverted ETH payout: held, then the bot swapped %d mHBD of the 50,000 back to Hive (floor %d); confirmSpend %d RC, swap back %d RC", back, floor, cs.RcUsed, rr.RcUsed)
		} else if back != 0 {
			t.Fatal("a paid payout moved the pool")
		}
	}
}

// poETHWorld is rwUSDCBridge plus the router (registered with the bridge) and
// an ETH/HBD pool of 1 ETH / 2,000 HBD, egl 60,000, and a trader holding 50
// HBD (+ RC headroom). It returns the LP account.
func poETHWorld(t *testing.T) (rwChain, string) {
	t.Helper()
	router, pool := os.Getenv("DEX_ROUTER_WASM"), os.Getenv("DEX_POOL_WASM")
	if router == "" || pool == "" {
		t.Skip("DEX_ROUTER_WASM / DEX_POOL_WASM not set")
	}
	rw, _ := os.ReadFile(router)
	pw, _ := os.ReadFile(pool)
	c, lp := rwUSDCBridge(t)
	ct := c.ct
	ct.RegisterContract(poRouter, "hive:owner", rw)
	ct.RegisterContract(poPool, "hive:owner", pw)
	ct.SetPendulumGeometry(pendulumoracle.GeometryOutputs{OK: true, V: 500_000, P: 250_000, E: 1_000_000, T: 1_000_000, SBps: 5000}, []string{poPool})
	ct.StateSet(rwBridge, "routerid", poRouter)
	ct.StateSet(rwBridge, "egl", "60000")
	ct.StateSet(rwBridge, "a-"+lp+"-eth", "1000000000000000000")
	ct.StateSet(rwBridge, "s-eth", "1100000000000000000|1000000000000000000|0|0") // 1 ETH user + 0.1 reserve
	for _, step := range []struct{ contract, action, payload string }{
		{poRouter, "init", ""},
		{poRouter, "register_token", `{"name":"HBD","chain":"HIVE"}`},
		{poRouter, "register_token", `{"name":"eth","chain":"ETH","mapping_contract":"` + rwBridge + `"}`},
		{poRouter, "register_pool", `{"asset0":"eth","asset1":"hbd","dex_contract_id":"` + poPool + `"}`},
		{poPool, "init", `{"asset0":"eth","asset1":"hbd","fee_bps":8,"asset0_mapping_contract":"` + rwBridge + `","router_contract":"` + poRouter + `"}`},
	} {
		if r := poCall(ct, "hive:owner", step.contract, step.action, step.payload, 20_000, nil); !r.Success {
			t.Fatalf("%s %s: %s: %s", step.contract, step.action, r.Err, r.ErrMsg)
		}
	}
	if r := rwCall(ct, lp, "approve", []byte(`{"spender":"contract:`+poPool+`","amount":"1000000000000000000","asset":"eth"}`)); !r.Success {
		t.Fatalf("approve: %s: %s", r.Err, r.ErrMsg)
	}
	ct.Deposit(lp, 2_000_000, ledgerDb.AssetHbd)
	if r := poCall(ct, lp, poPool, "add_liquidity", `{"amount0":"1000000000000000000","amount1":"2000000","recipient":"`+lp+`"}`, 20_000, poAllow("hbd", "2000.000")); !r.Success {
		t.Fatalf("add_liquidity: %s: %s", r.Err, r.ErrMsg)
	}
	ct.Deposit(poTrader, 50_000+poHeadroom, ledgerDb.AssetHbd)
	rwHeaderAt(ct, 102, &fakeChain{}, rwBlockTime)
	return c, lp
}

// An Ethereum deposit that asks for a swap (calldata swap_to / asset_out /
// min_amount_out) runs through the bridge's routeDeposit and the real router,
// the path the BTC and EVM deposit swaps share (a contract caller, a mapped
// input, return_address MAGI). Done: the recipient gets the HBD. Failed (min
// out not met): the ETH is owed home, listed, and the keeper's sendHome owed=1
// sends exactly that back to the depositor's address as a payout; a plain
// deposit by the same address is not touched.
func TestRealWasm_PAYOUT1_EthereumDepositSwap(t *testing.T) {
	for _, fail := range []bool{false, true} {
		c, _ := poETHWorld(t)
		ct := c.ct
		ethKey, _ := ethCrypto.GenerateKey()
		depositor := ethCrypto.PubkeyToAddress(ethKey.PublicKey)
		did := rwDID(depositor)
		min := "1"
		if fail {
			min = "999999999"
		}
		chain := &fakeChain{}
		data := []byte("swap_to=hive:po-dest&asset_out=hbd&min_amount_out=" + min)
		tx := chain.addTx(t, ethKey, &types.DynamicFeeTx{
			GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(30e9), Gas: 60000, To: &c.vault, Value: big.NewInt(5e16), Data: data,
		})
		rwHeaderAt(ct, 102, chain, rwBlockTime)
		dep := detectedDeposit{BlockHeight: 102, TxIndex: 0, LogIndex: -1, TxHash: tx.Hash().Hex(), DepositType: "eth"}
		r := rwCallLimit(ct, "hive:anyone", "map", buildMapPayloadFromRPC(context.Background(), chain.rpc(t), dep, dep.BlockHeight), 60_000)
		if !r.Success {
			t.Fatalf("fail=%v: map: %s: %s", fail, r.Err, r.ErrMsg)
		}
		t.Logf("fail=%v: map + swap %d RC; bridge logs %v", fail, r.RcUsed, poLogs(r, rwBridge))
		if !fail {
			if got := ct.GetBalance("hive:po-dest", ledgerDb.AssetHbd); got <= 0 {
				t.Fatalf("the swap's recipient got %d HBD", got)
			}
			if ct.StateGet(rwBridge, "ohl") != "" {
				t.Fatal("a swap that ran was listed as owed home")
			}
			continue
		}
		if !poHasLog(r, rwBridge, "swapfail|t="+did+"|a=eth|d=50000000000000000|") || !poHasLog(r, rwBridge, "owedhome|t="+did+"|a=eth|d=50000000000000000") {
			t.Fatalf("logs %v", poLogs(r, rwBridge))
		}
		if l := ct.StateGet(rwBridge, "ohl"); l != "eth|"+did {
			t.Fatalf("ohl %q, want the depositor listed", l)
		}
		// Held, never on the depositor's Magi balance (owner 11-21).
		if b := ct.StateGet(rwBridge, "a-"+did+"-eth"); b != "" && b != "0" {
			t.Fatalf("the failed swap's ETH sits on the depositor's Magi balance: %s", b)
		}
		if h := ct.StateGet(rwBridge, "a-system:owed-home-eth"); h != "50000000000000000" {
			t.Fatalf("owed-home hold %q, want the 0.05 ETH", h)
		}
		// The keeper's call, as send_home.go builds it.
		payload, _ := json.Marshal(owedHomeCall("eth|" + did))
		var raw string
		_ = json.Unmarshal(payload, &raw)
		rwHeaderAt(ct, 103, &fakeChain{}, rwBlockTime)
		ct.Deposit("hive:keeper-bot", 1_000_000, ledgerDb.AssetHbd) // the bot holds RC (a withdrawal is over the free 10k)
		ct.StateSet(rwBridge, "rl-hive:keeper-bot", "1") // the bridge's bot sends owed refunds
		sh := rwCallLimit(ct, "hive:keeper-bot", "sendHome", []byte(raw), 60_000)
		if !sh.Success || !strings.HasPrefix(sh.Ret, "issued:") {
			t.Fatalf("sendHome owed=1: %s: %s %q", sh.Err, sh.ErrMsg, sh.Ret)
		}
		ps := rwPending(t, c, 0)
		fee := new(big.Int).Mul(big.NewInt(60_000), big.NewInt(3_500_000_000))
		pf, _ := new(big.Int).SetString(ct.StateGet(rwBridge, "pf-eth"), 10)
		if pf == nil {
			pf = new(big.Int)
		}
		want := new(big.Int).Sub(new(big.Int).Sub(big.NewInt(5e16), pf), fee)
		if !strings.EqualFold(ps.To, depositor.Hex()) || ps.Amount.Cmp(want) != 0 {
			t.Fatalf("sent %s to %s, want %s to the depositor %s", ps.Amount, ps.To, want, depositor.Hex())
		}
		if h := ct.StateGet(rwBridge, "a-system:owed-home-eth"); h != "" && h != "0" {
			t.Fatalf("the hold keeps %s after the send", h)
		}
		if ct.StateGet(rwBridge, "ohl") != "" || ct.StateGet(rwBridge, "oh-eth-"+did) != "" {
			t.Fatalf("owed record left: ohl %q oh %q", ct.StateGet(rwBridge, "ohl"), ct.StateGet(rwBridge, "oh-eth-"+did))
		}
		t.Logf("owed refund sent home: %s wei to %s (payout fee %s), sendHome %d RC", ps.Amount, ps.To, fee, sh.RcUsed)
	}
}

// Every way a Hive-paid swap paid out on Ethereum can fail inside the call goes
// back to the trader on Hive: nothing in the Magi balance, on the router, in the
// pool or queued on the bridge.
func TestRealWasm_PAYOUT1_HivePaidPayoutFailuresGoBackToHive(t *testing.T) {
	cases := []struct {
		name   string
		swap   string
		amount string
		setup  func(ct *test_utils.ContractTest)
		why    string
	}{
		{"slippage", `"min_amount_out":"999999999","recipient":"` + poTo + `"`, "5000", nil, "slippage"},
		{"below the bridge minimum", `"min_amount_out":"1","recipient":"` + poTo + `"`, "800", nil, "minimum withdrawal"},
		{"bad Ethereum address", `"min_amount_out":"1","recipient":"0x1234"`, "5000", nil, "address"},
		{"bridge paused", `"min_amount_out":"1","recipient":"` + poTo + `"`, "5000",
			func(ct *test_utils.ContractTest) { ct.StateSet(rwBridge, "paused", "1") }, "paused"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := poWorld(t)
			ct := c.ct
			if tc.setup != nil {
				tc.setup(ct)
			}
			before := ct.GetBalance(poTrader, ledgerDb.AssetHbd)
			poolUsdc := ct.StateGet(rwBridge, "a-contract:"+poPool+"-usdc")
			swap := `{"type":"swap","version":"1.0.0","asset_in":"hbd","asset_out":"usdc","amount_in":"` + tc.amount + `",` + tc.swap +
				`,"destination_chain":"ETH","return_address":{"chain":"HIVE","address":"` + poTrader + `"}}`
			r := poCall(ct, poTrader, poRouter, "execute", swap, 60_000, poAllow("hbd", "5.000"))
			if !r.Success {
				t.Fatalf("must succeed with the input sent back: %s: %s", r.Err, r.ErrMsg)
			}
			if !poHasLog(r, poRouter, "swaprefund|t="+poTrader+"|a=hbd|d="+tc.amount+"|why=") {
				t.Fatalf("router logs %v", poLogs(r, poRouter))
			}
			why := ""
			for _, l := range poLogs(r, poRouter) {
				if strings.HasPrefix(l, "swaprefund|") {
					why = l
				}
			}
			if !strings.Contains(strings.ToLower(why), strings.ToLower(tc.why)) {
				t.Logf("refund reason %q (expected to mention %q)", why, tc.why)
			}
			amt, _ := strconv.ParseInt(tc.amount, 10, 64)
			if got := ct.GetBalance(poTrader, ledgerDb.AssetHbd); got != before-amt {
				t.Fatalf("trader Magi HBD %d, want %d: the input must leave Magi (withdrawn to Hive)", got, before-amt)
			}
			if got := ct.GetBalance("contract:"+poRouter, ledgerDb.AssetHbd); got != 0 {
				t.Fatalf("router HBD %d", got)
			}
			if got := ct.StateGet(rwBridge, "a-contract:"+poPool+"-usdc"); got != poolUsdc {
				t.Fatalf("pool USDC %s -> %s", poolUsdc, got)
			}
			if ct.StateGet(rwBridge, "d-0") != "" || ct.StateGet(rwBridge, "wqt") != "" {
				t.Fatal("a payout was issued or queued")
			}
			t.Logf("%s: back to Hive, %d RC; %s", tc.name, r.RcUsed, why)
		})
	}
}

// The residual the router cannot cover: a call that runs out of RC aborts as a
// whole, before the swap back, and the deposit stays in the Magi balance.
// Altera refuses such a swap before signing (hivePaidSwapRcShortfall; payouts
// sized from the account's RC). Kept as a test so the residual stays visible.
func TestRealWasm_PAYOUT1_OutOfRCIsTheResidual(t *testing.T) {
	c := poWorld(t)
	ct := c.ct
	before := ct.GetBalance(poTrader, ledgerDb.AssetHbd)
	r := poCall(ct, poTrader, poRouter, "execute", poSwap(true), 5_000, poAllow("hbd", "5.000"))
	if r.Success {
		t.Fatal("expected the call to run out of RC")
	}
	if got := ct.GetBalance(poTrader, ledgerDb.AssetHbd); got != before {
		t.Fatalf("trader HBD %d, want %d untouched", got, before)
	}
	t.Logf("out of RC at rc_limit 5,000: %s; the 5 HBD deposit stays on Magi (Altera refuses before signing)", r.Err)
}

// An ETH payout carries the gas limit the wallet sized for its recipient
// (estimate + 20%): the bridge signs and charges exactly those units. A limit
// below a plain transfer's is refused by the bridge and the swap goes back to
// Hive.
func TestRealWasm_PAYOUT1_ETHPayoutCarriesItsGasLimit(t *testing.T) {
	for _, limit := range []uint64{25_200, 20_000} {
		c, _ := poETHWorld(t)
		ct := c.ct
		swap := `{"type":"swap","version":"1.0.0","asset_in":"hbd","asset_out":"eth","amount_in":"50000","min_amount_out":"1","recipient":"` + poTo +
			`","destination_chain":"ETH","payout_gas_limit":` + strconv.FormatUint(limit, 10) + `,"return_address":{"chain":"HIVE","address":"` + poTrader + `"}}`
		r := poCall(ct, poTrader, poRouter, "execute", swap, 60_000, poAllow("hbd", "50.000"))
		if !r.Success {
			t.Fatalf("limit %d: %s: %s", limit, r.Err, r.ErrMsg)
		}
		if limit < 21_000 {
			if !poHasLog(r, poRouter, "swaprefund|t="+poTrader+"|a=hbd|d=50000|") {
				t.Fatalf("a limit below 21,000 was not refused: %v", poLogs(r, poRouter))
			}
			continue
		}
		ps := rwPending(t, c, 0)
		if g := rwTxGas(t, ps); g != limit {
			t.Fatalf("signed gas limit %d, want %d", g, limit)
		}
		fee := new(big.Int).Mul(new(big.Int).SetUint64(limit), big.NewInt(3_500_000_000))
		found := false
		for _, l := range poLogs(r, rwBridge) {
			if strings.HasPrefix(l, "unmap|n=0|") && strings.Contains(l, "|g="+fee.String()+"|gs=user") {
				found = true
			}
		}
		if !found {
			t.Fatalf("unmap log does not show %s wei for %d gas: %v", fee, limit, poLogs(r, rwBridge))
		}
		t.Logf("payout gas limit %d: fee %s wei (vs %s at egl 60,000)", limit, fee, new(big.Int).Mul(big.NewInt(60_000), big.NewInt(3_500_000_000)))
	}
}

// poBotRetry runs retryPayoutReturn as the bridge's bot: rwRelayer, registered
// as a relayer (deploy wiring: registerRelayer through the timelocked admin
// path), at min_out 1 (the bridge keeps the floor for the first day).
func poBotRetry(ct *test_utils.ContractTest, id uint64) test_utils.ContractTestCallResult {
	ct.StateSet(rwBridge, "rl-"+rwRelayer, "1")
	return rwCallLimit(ct, rwRelayer, "retryPayoutReturn", []byte(fmt.Sprintf(`{"id":%d,"min_out":"1"}`, id)), 60_000)
}
