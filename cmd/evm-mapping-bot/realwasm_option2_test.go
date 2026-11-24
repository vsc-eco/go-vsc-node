//go:build evm_realwasm

package main

// OPTION-2 (owner 11-21: deliver what the swap bought) end to end on the
// node's wasm runtime: the DEX router, an ETH/HBD pool and the EVM bridge. A
// Hive-paid swap's ETH payout that a roll keeps from L1 (the roll's 0-value
// self-transfer spent its nonce, so it can never mine) is sent again to the
// same address, paying its own gas out of the payout; the third send
// that a roll keeps from L1 is held for the trader and the bridge's bot swaps it
// back to Hive with its floor. Nothing lands on a Magi balance at any step.
//
//	EVM_MAPPING_WASM=<bridge main.wasm> DEX_ROUTER_WASM=<dex-router-v2.wasm> DEX_POOL_WASM=<dex.wasm> \
//	  go test -tags evm_realwasm ./cmd/evm-mapping-bot/ -run RealWasm_OPTION2 -v

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/core/types"

	ledgerDb "vsc-node/modules/db/vsc/ledger"
)

func TestRealWasm_OPTION2_RolledETHPayoutIsSentAgainThenGoesBack(t *testing.T) {
	c, _ := poETHWorld(t)
	ct := c.ct
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
	nothingOnMagi := func(label string) {
		t.Helper()
		if e := ct.StateGet(rwBridge, "a-"+poTrader+"-eth"); e != "" && e != "0" {
			t.Fatalf("%s: trader holds %s wei on Magi", label, e)
		}
		if got := ct.GetBalance(poTrader, ledgerDb.AssetHbd); got != poHeadroom {
			t.Fatalf("%s: trader Magi HBD %d, want only the RC headroom %d", label, got, poHeadroom)
		}
		if e := ct.StateGet(rwBridge, "a-contract:"+poRouter+"-eth"); e != "" && e != "0" {
			t.Fatalf("%s: the router holds %s wei", label, e)
		}
	}
	swap := `{"type":"swap","version":"1.0.0","asset_in":"hbd","asset_out":"eth","amount_in":"50000","min_amount_out":"1","recipient":"` + poTo + `","destination_chain":"ETH","return_address":{"chain":"HIVE","address":"` + poTrader + `"}}`
	if r := poCall(ct, poTrader, poRouter, "execute", swap, 60_000, poAllow("hbd", "50.000")); !r.Success {
		t.Fatalf("swap: %s: %s", r.Err, r.ErrMsg)
	}
	first := rwPending(t, c, 0)
	var raw map[string]any
	_ = json.Unmarshal([]byte(ct.StateGet(rwBridge, "d-0")), &raw)
	floor, _ := strconv.ParseInt(fmt.Sprint(raw["return_min"]), 10, 64)
	if raw["payout"] != true || floor <= 0 {
		t.Fatalf("issued payout %s: want payout=true and a floor", ct.StateGet(rwBridge, "d-0"))
	}
	books("after the swap")
	l1 := uint64(102)
	// Every send pays its own gas out of the payout (60,000 x 3.5 gwei here):
	// the reserve only lends it.
	sendGas := new(big.Int).Mul(big.NewInt(60_000), big.NewInt(3_500_000_000))
	expect := new(big.Int).Set(first.Amount)
	for n := uint64(0); n < 3; n++ {
		ps := rwPending(t, c, n)
		if ps.Amount.Cmp(expect) != 0 || !strings.EqualFold(ps.To, poTo) {
			t.Fatalf("send %d: %s to %s, want %s to %s", n+1, ps.Amount, ps.To, expect, poTo)
		}
		expect.Sub(expect, sendGas)
		l1 += 1200
		rwHeaderAt(ct, l1, &fakeChain{}, rwBlockTime)
		if r := rwCall(ct, "hive:carol", "rollWithdrawal", []byte(fmt.Sprintf(`{"nonce":%d}`, n))); !r.Success {
			t.Fatalf("roll %d: %s: %s", n, r.Err, r.ErrMsg)
		}
		rolled := rwPending(t, c, n)
		l1++
		chain, tx := rwMine(t, c, rolled, l1, types.ReceiptStatusSuccessful)
		payload, err := buildConfirmSpendPayload(context.Background(), chain.rpc(t), tx.Hash().Hex(), l1, 0, rolled)
		if err != nil {
			t.Fatal(err)
		}
		poolHBD := ct.GetBalance("contract:"+poPool, ledgerDb.AssetHbd)
		reserve := ct.StateGet(rwBridge, "gr")
		// The bot sizes confirmSpend from its dry run (x2; past the dry run's
		// cap, vaultOpRcLimit); a re-send issued inside it costs more than a
		// plain settlement, so it is sent here with room and its cost logged.
		cs := rwCallLimit(ct, rwRelayer, "confirmSpend", payload, 100_000)
		if !cs.Success {
			t.Fatalf("confirmSpend of roll %d: %s: %s", n, cs.Err, cs.ErrMsg)
		}
		books(fmt.Sprintf("after roll %d", n))
		back := poolHBD - ct.GetBalance("contract:"+poPool, ledgerDb.AssetHbd)
		if n < 2 {
			next := rwPending(t, c, n+1)
			var nraw map[string]any
			_ = json.Unmarshal([]byte(ct.StateGet(rwBridge, fmt.Sprintf("d-%d", n+1))), &nraw)
			if !poHasLog(cs, rwBridge, fmt.Sprintf("payoutresend|n=%d|", n)) || next == nil || next.Amount.Cmp(expect) != 0 ||
				!strings.EqualFold(next.To, poTo) || fmt.Sprint(nraw["attempt"]) != strconv.Itoa(int(n)+1) || back != 0 {
				t.Fatalf("roll %d: want the same payout sent again (attempt %d), pool untouched (%d back); d-%d %s; logs %v",
					n, n+1, back, n+1, ct.StateGet(rwBridge, fmt.Sprintf("d-%d", n+1)), poLogs(cs, rwBridge))
			}
			t.Logf("roll %d: sent again as nonce %d (%s wei to %s), reserve %s -> %s, confirmSpend %d RC", n, n+1, next.Amount, next.To, reserve, ct.StateGet(rwBridge, "gr"), cs.RcUsed)
		} else {
			// Out of sends: held for the trader (nothing swapped inside the
			// confirmSpend anyone can wrap), then the bot swaps it back to Hive.
			if !poHasLog(cs, rwBridge, "payoutreturnheld|id=0|n=2|") || back != 0 || ct.StateGet(rwBridge, "d-3") != "" {
				t.Fatalf("third roll: want it held, no fourth send; %d mHBD moved; logs %v", back, poLogs(cs, rwBridge))
			}
			rr := poBotRetry(ct, 0)
			back = poolHBD - ct.GetBalance("contract:"+poPool, ledgerDb.AssetHbd)
			if !rr.Success || back < floor {
				t.Fatalf("bot's swap back: %s: %s, %d back (floor %d)", rr.Err, rr.ErrMsg, back, floor)
			}
			t.Logf("third roll: held, then the bot swapped %d mHBD of the 50,000 back to Hive (floor %d); confirmSpend %d RC", back, floor, cs.RcUsed)
		}
		nothingOnMagi(fmt.Sprintf("after roll %d", n))
	}
}
