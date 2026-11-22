//go:build evm_devnet

// SWAP-SAFETY (owner 11-14: "when swapping we cannot have funds get stuck on
// magi in any case. it is refunded to sender or fails safe"): every refund path
// of the BTC mapping contract, the router and the EVM bridge, together on one
// devnet with the DEX3 pools. A deposit swap completes, goes back to its sender
// (refund_to), or is held for a manual refund; never to swap_to, never left on a
// contract, never on a 0-RC account without sendHome.
//
// Needs the BOTH inputs (BTC_MAPPING_WASM_PATH, the EVM artifacts) and DEX_WASM /
// DEX_ROUTER_WASM, all built from the swap-safety branches.
//
// Run: go test -tags evm_devnet -run TestSwapSafety ./tests/devnet/ -timeout 240m -v
package devnet

import (
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"vsc-node/cmd/mapping-bot/chain"

	"github.com/btcsuite/btcd/chaincfg"
	ethcommon "github.com/ethereum/go-ethereum/common"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

const (
	ssPayee  = "hive:magi.test2" // the swap's recipient on Hive (a third party)
	ssSender = "hive:magi.test3" // the sender's own hive: account (refund_to)
	ssHold   = "system:refund-hold"
)

// ssDeposit sends sats on regtest to instruction's deposit address and maps it
// (the harness plays the mapping bot). Returns the deposit's outpoint label.
func ssDeposit(t *testing.T, x *dex3, instruction string, sats int64) string {
	t.Helper()
	b := x.b
	d, ctx, cid := b.d, b.ctx, b.v.cid
	gen := &chain.BTCAddressGenerator{Params: &chaincfg.RegressionNetParams, BackupCSVBlocks: 2}
	addr, _, err := gen.GenerateDepositAddress(b.v.primary0, backupPubKeyG, instruction)
	if err != nil {
		t.Fatalf("deposit address: %v", err)
	}
	amt := fmt.Sprintf("%d.%08d", sats/1e8, sats%int64(1e8))
	txid, err := d.bitcoinCli(ctx, "sendtoaddress", addr, amt)
	if err != nil {
		t.Fatalf("sendtoaddress: %v", err)
	}
	txid = strings.TrimSpace(txid)
	h, err := d.MineBlocks(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	bhash, _ := d.bitcoinCli(ctx, "getblockhash", fmt.Sprint(h))
	blockJSON, _ := d.bitcoinCli(ctx, "getblock", strings.TrimSpace(bhash), "1")
	var blk struct {
		Tx []string `json:"tx"`
	}
	if err := json.Unmarshal([]byte(blockJSON), &blk); err != nil || len(blk.Tx) != 2 {
		t.Fatalf("deposit block %d: want coinbase + deposit, got %v (%v)", h, blk.Tx, err)
	}
	rawTx, _ := d.bitcoinCli(ctx, "getrawtransaction", txid)
	rawTx = strings.TrimSpace(rawTx)
	vout := ssVout(t, x, txid, addr)
	if _, err := d.MineBlocks(ctx, vfDepositMaturityBlocks); err != nil {
		t.Fatal(err)
	}
	last := contractLastHeight(t, d, ctx, cid)
	for hh := last + 1; hh <= h+uint64(vfDepositMaturityBlocks); hh++ {
		hx, _ := btcBlockHeaderHex(ctx, d, hh)
		if s := vstatus(t, d, ctx, 1, cid, "addBlocks", fmt.Sprintf(`{"blocks":"%s","latest_fee":10}`, hx)); !isOK(s) {
			t.Logf("addBlocks %d: %s (the oracle may have relayed it)", hh, s)
		}
	}
	payload := fmt.Sprintf(`{"tx_data":{"block_height":%d,"raw_tx_hex":"%s","merkle_proof_hex":"%s","tx_index":1},"instructions":["%s"]}`,
		h, rawTx, reverseHexBytes(blk.Tx[0]), instruction)
	st, mapTx := vstatusTx(t, d, ctx, 1, cid, "map", payload)
	if !isOK(st) {
		t.Fatalf("map of %s: %s", instruction, st)
	}
	t.Logf("SS map %s -> %s (%d sats, %s:%d): %s", instruction, addr, sats, txid, vout, vfCallResult(d, ctx, 1, mapTx))
	return txid + ":" + strconv.Itoa(vout)
}

func ssVout(t *testing.T, x *dex3, txid, addr string) int {
	t.Helper()
	raw, err := x.b.d.bitcoinCli(x.b.ctx, "getrawtransaction", txid, "true")
	if err != nil {
		t.Fatal(err)
	}
	var tx struct {
		Vout []struct {
			N            int `json:"n"`
			ScriptPubKey struct {
				Address string `json:"address"`
			} `json:"scriptPubKey"`
		} `json:"vout"`
	}
	if err := json.Unmarshal([]byte(raw), &tx); err != nil {
		t.Fatal(err)
	}
	for _, o := range tx.Vout {
		if o.ScriptPubKey.Address == addr {
			return o.N
		}
	}
	t.Fatalf("no output to %s in %s", addr, txid)
	return -1
}

func (x *dex3) btcState(key string) string {
	m, err := x.b.d.GetStateByKeys(x.b.ctx, x.b.e.gqlNode, x.b.v.cid, []string{key})
	if err != nil || m == nil || m[key] == nil {
		return ""
	}
	return fmt.Sprint(m[key])
}

func (x *dex3) sats(account string) int64 {
	return balanceSats(x.b.t, x.b.d, x.b.ctx, x.b.v.cid, account)
}

// ssWaitSats waits for account's BTC balance on the mapping contract to equal want.
func (x *dex3) ssWaitSats(account string, want int64, within time.Duration) bool {
	start := devnetNow()
	for devnetSince(start) < within {
		if x.sats(account) == want {
			return true
		}
		devnetSleep(10 * time.Second)
	}
	return false
}

func TestSwapSafety_EveryRefundPathTogether(t *testing.T) {
	// SS_SLIP_PARITY=1 runs the pools on THORChain's fee rule from block 1
	// (params.ConsensusParams.PendulumSlipParityHeight); unset, the rule stays
	// inert and the old fee applies.
	parity := os.Getenv("SS_SLIP_PARITY") == "1"
	var opts []func(*Config)
	if parity {
		opts = append(opts, func(c *Config) { c.SysConfigOverrides.ConsensusParams.PendulumSlipParityHeight = 1 })
	}
	x := newBothEnvDEX3(t, 230*time.Minute, opts...)
	x.setup()
	prFundReserve(t, x) // payouts borrow their gas gap from it (mainnet's is funded)
	b := x.b
	d, ctx, cid, e := b.d, b.ctx, b.v.cid, b.e
	// Deposit swaps need the router on the BTC contract (owner, node 1).
	x.call(cid, "registerRouter", fmt.Sprintf(`{"router_contract":"%s"}`, x.router), nil, 0)

	const impossible = "999999999999"
	fails := 0
	check := func(ok bool, format string, a ...any) {
		if !ok {
			fails++
			t.Errorf("SS FAIL: "+format, a...)
		} else {
			t.Logf("SS ok: "+format, a...)
		}
	}

	// S1: a deposit swap pays HBD to a third party's Hive account. The pool holds
	// 0.01 BTC : 50 HBD, so 0.001 BTC buys at most 50*0.001/0.011 = 4.545 HBD
	// before fees; anything outside [4.50, 4.545] is an overcharge or a bug.
	// Under THORChain's rule the fee is the full slip fee x²·Y/(x+X)² = 0.413 HBD
	// (the protocol tier, ~0.007, is smaller), so the payout is 4.132.
	lo, hi := int64(4_500), int64(4_545)
	if parity {
		lo, hi = 4_125, 4_140
	}
	l1Before := gwL1Hbd(t, d, strings.TrimPrefix(ssPayee, "hive:"))
	ssDeposit(t, x, "swap_to=hive%3Amagi.test2&swap_asset_out=hbd&destination_chain=HIVE&min_amount_out=1&refund_to=hive%3Amagi.test3", 100_000)
	paid := int64(0)
	for i := 0; i < 60 && paid == 0; i++ {
		devnetSleep(10 * time.Second)
		paid = gwL1Hbd(t, d, strings.TrimPrefix(ssPayee, "hive:")) - l1Before
	}
	check(paid >= lo && paid <= hi, "S1 Hive payout %d milli-HBD for 0.001 BTC (want [%d, %d], parity %v)", paid, lo, hi, parity)
	check(x.sats(ssSender) == 0, "S1 nothing refunded when the swap completes (%d)", x.sats(ssSender))

	// S2 (SENDER-REFUND-1): the swap cannot meet its minimum; the sender is
	// proven, so the BTC goes back to the address that sent it on Bitcoin (a
	// hive: refund_to is no longer credited on Magi).
	prS2BitcoinSenderRefund(t, x, check)

	// S3: refund to the sender's own Bitcoin address, returned on Bitcoin by a
	// third party's sendHome.
	refundAddr, _ := d.bitcoinCli(ctx, "getnewaddress", "", "bech32")
	refundAddr = strings.TrimSpace(refundAddr)
	ssDeposit(t, x, "swap_to=hive%3Amagi.test2&swap_asset_out=hbd&destination_chain=HIVE&min_amount_out="+impossible+"&refund_to="+refundAddr, 60_000)
	check(x.btcState("rf-"+refundAddr) == "60000", "S3 refundable recorded for %s (%q)", refundAddr, x.btcState("rf-"+refundAddr))
	before := txSpendIds(t, d, ctx, cid)
	// node 2: not the owner, not the sender. The address goes as a JSON string, as
	// the mapping bot's keeper sends it (cmd/mapping-bot/mapper/send_home.go).
	st := vstatus(t, d, ctx, 2, cid, "sendHome", `"`+refundAddr+`"`)
	check(isOK(st), "S3 sendHome by a third party: %s", st)
	if isOK(st) {
		x.btcCompletePending(before, b.btcKeyID(0))
		got, _ := d.bitcoinCli(ctx, "getreceivedbyaddress", refundAddr, "0")
		f, _ := strconv.ParseFloat(strings.TrimSpace(got), 64)
		recv := int64(f*1e8 + 0.5)
		check(recv > 55_000 && recv <= 60_000, "S3 %d sats arrived at %s on Bitcoin (60000 less fees)", recv, refundAddr)
	}

	// S4: a legacy instruction (no refund_to) that fails is held, never paid to
	// swap_to; only the owner releases it.
	op := ssDeposit(t, x, "swap_to=hive%3Amagi.test2&swap_asset_out=hbd&destination_chain=HIVE&min_amount_out="+impossible, 40_000)
	check(x.ssWaitSats(ssHold, 40_000, 3*time.Minute), "S4 held (%d sats on %s)", x.sats(ssHold), ssHold)
	check(x.sats(ssPayee) == 0, "S4 swap_to got nothing (%d)", x.sats(ssPayee))
	st = vstatus(t, d, ctx, 2, cid, "releaseHold", "o="+op+"&to=hive%3Amagi.test3")
	check(!isOK(st), "S4 a non-owner cannot release a hold (%s)", st)
	senderBefore := x.sats(ssSender)
	st = vstatus(t, d, ctx, 1, cid, "releaseHold", "o="+op+"&to="+ssSender)
	check(isOK(st), "S4 owner releases the hold: %s", st)
	check(x.ssWaitSats(ssSender, senderBefore+40_000, 3*time.Minute), "S4 released to the sender (%d)", x.sats(ssSender))

	// S5: a swap settled to Ethereum whose L1 withdrawal fails (the recipient is
	// the MockUSDC contract, which refuses ETH): the refund goes to return_address,
	// not to the router's caller.
	if b.usdc != (ethcommon.Address{}) {
		retBefore := strings.SplitN(e.stateStr("a-"+ssSender+"-eth"), "|", 2)[0]
		instr := fmt.Sprintf(`{"type":"swap","version":"1.0.0","asset_in":"hbd","asset_out":"eth","amount_in":"2000","recipient":"%s","destination_chain":"ETH","max_fee":3000000000000000,"return_address":{"chain":"MAGI","address":"%s"}}`,
			strings.ToLower(b.usdc.Hex()), ssSender)
		x.call(x.router, "execute", instr, hbdAllow(2000), 50_000)
		err := b.botPoll(30*time.Minute, func() bool {
			v := strings.SplitN(e.stateStr("a-"+ssSender+"-eth"), "|", 2)[0]
			return v != "" && v != retBefore
		})
		check(err == nil, "S5 failed ETH payout refunded to return_address %s (eth %q -> %q)", ssSender, retBefore, strings.SplitN(e.stateStr("a-"+ssSender+"-eth"), "|", 2)[0])
	}

	// S6: a CEX-style ETH deposit (no calldata) lands on the sender's own 0-RC
	// account; a third party's sendHome returns it to the sending address.
	cexKey := mustKey(t)
	cexAddr := ethCrypto.PubkeyToAddress(cexKey.PublicKey)
	if err := b.anvil.SetBalance(ctx, cexAddr, bbEth(2)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.anvil.SendETH(ctx, cexKey, b.vault0, bbEth(1)); err != nil {
		t.Fatal(err)
	}
	cexDID := evmDID(cexAddr)
	err := b.botPoll(20*time.Minute, func() bool { return e.stateStr("rf-eth-"+cexDID) != "" })
	check(err == nil, "S6 plain ETH deposit recorded refundable for %s (%q)", cexDID, e.stateStr("rf-eth-"+cexDID))
	if err == nil {
		l1Before := b.l1Balance(cexAddr)
		id, cerr := d.CallContract(ctx, 2, e.amID, "sendHome", "address="+strings.ToLower(cexAddr.Hex())+"&asset=eth")
		ok, msg := false, fmt.Sprint(cerr)
		if cerr == nil {
			ok, msg = e.outResult(ctx, id)
		}
		check(ok, "S6 sendHome by a third party: %s", msg)
		werr := b.botPoll(30*time.Minute, func() bool { return b.l1Balance(cexAddr).Cmp(l1Before) > 0 })
		got := new(big.Int).Sub(b.l1Balance(cexAddr), l1Before)
		check(werr == nil && got.Cmp(new(big.Int).Div(bbEth(9), big.NewInt(10))) > 0, "S6 %s wei back at the sending address (deposit 1 ETH less fees)", got)
	}

	// The final payout rules on the EVM side: the router wired on the bridge
	// the way mainnet must (timelocked), a refused Hive-paid payout held and
	// swapped back by the bot only, a failed Ethereum deposit swap sent home by
	// the bot only, and a stuck payout rolled and sent again.
	prS0WireRouter(t, x, check)
	prS8HivePaidPayoutRefused(t, x, check)
	prS9EthereumDepositSwapOwedHome(t, x, check)
	prS10RolledPayoutResent(t, x, check)

	// S7: nothing stays on a contract account.
	for _, acct := range []string{"contract:" + cid, "contract:" + x.router, ssHold} {
		check(x.sats(acct) == 0, "S7 BTC contract: %s holds %d sats", acct, x.sats(acct))
	}
	for _, k := range []string{"a-contract:" + x.router + "-eth", "a-contract:" + x.router + "-usdc", "a-contract:" + e.amID + "-eth",
		"a-system:payout-return-eth", "a-system:payout-return-usdc", "a-system:owed-home-eth"} {
		v := strings.SplitN(e.stateStr(k), "|", 2)[0]
		check(v == "" || v == "0", "S7 EVM bridge: %s = %q", k, v)
	}
	if fails == 0 {
		t.Logf("CASE SWAP-SAFETY PASS: S1-S7")
	} else {
		t.Logf("CASE SWAP-SAFETY FAIL: %d checks failed\n%s", fails, strings.Join(b.notes, "\n"))
	}
}
