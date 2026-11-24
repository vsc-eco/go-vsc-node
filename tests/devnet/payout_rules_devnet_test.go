//go:build evm_devnet

// PAYOUT RULES (owner 11-21/22: THORChain's rules on Magi; funds never lost,
// never stuck on a Magi balance, fees a little over at most, the reserve never
// drained), on a live devnet: real nodes, TSS signing, the running EVM bot as a
// registered relayer, anvil and regtest. Each case is checked from the user's
// side: where the value is at every step, what arrives, and how long it took.
// Called from TestSwapSafety_EveryRefundPathTogether (one devnet).
package devnet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

type prCheck func(ok bool, format string, a ...any)

// prPayWait is the budget for the final "the recipient was paid" poll. The
// default is the real one; PR_PAY_WAIT (a Go duration) shortens it for a
// diagnostic run, where the evidence wanted is the L1 dump that follows the
// poll, not another hour of waiting for a payment that is not coming.
func prPayWait(def time.Duration) time.Duration {
	if v := os.Getenv("PR_PAY_WAIT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}

// prMerkleBranch is the Bitcoin merkle branch of txids[idx] (display hex), as
// the BTC contract reads it: each sibling's 32 bytes in internal order.
func prMerkleBranch(txids []string, idx int) string {
	level := make([][]byte, len(txids))
	for i, id := range txids {
		b, _ := hex.DecodeString(id)
		for l, r := 0, len(b)-1; l < r; l, r = l+1, r-1 {
			b[l], b[r] = b[r], b[l]
		}
		level[i] = b
	}
	var out []byte
	for len(level) > 1 {
		if len(level)%2 == 1 {
			level = append(level, level[len(level)-1])
		}
		out = append(out, level[idx^1]...)
		next := make([][]byte, 0, len(level)/2)
		for i := 0; i < len(level); i += 2 {
			h := sha256.Sum256(append(append([]byte{}, level[i]...), level[i+1]...))
			h = sha256.Sum256(h[:])
			next = append(next, h[:])
		}
		level, idx = next, idx/2
	}
	return hex.EncodeToString(out)
}

// prBlockTxs is the txids of the regtest block at height h.
func prBlockTxs(t *testing.T, x *dex3, h uint64) []string {
	t.Helper()
	d, ctx := x.b.d, x.b.ctx
	bhash, _ := d.bitcoinCli(ctx, "getblockhash", fmt.Sprint(h))
	blockJSON, _ := d.bitcoinCli(ctx, "getblock", strings.TrimSpace(bhash), "1")
	var blk struct {
		Tx []string `json:"tx"`
	}
	if err := json.Unmarshal([]byte(blockJSON), &blk); err != nil {
		t.Fatalf("block %d: %v", h, err)
	}
	return blk.Tx
}

// prRelayHeaders stores the regtest headers the BTC contract lacks up to h.
func prRelayHeaders(t *testing.T, x *dex3, h uint64) {
	t.Helper()
	d, ctx, cid := x.b.d, x.b.ctx, x.b.v.cid
	last := contractLastHeight(t, d, ctx, cid)
	for hh := last + 1; hh <= h; hh++ {
		hx, _ := btcBlockHeaderHex(ctx, d, hh)
		if s := vstatus(t, d, ctx, 1, cid, "addBlocks", fmt.Sprintf(`{"blocks":"%s","latest_fee":10}`, hx)); !isOK(s) {
			t.Logf("addBlocks %d: %s (the oracle may have relayed it)", hh, s)
		}
	}
}

// prS2BitcoinSenderRefund: a BTC swap that cannot meet its minimum, from a
// sender whose address the bot proves (proveSender, before the map, as the
// mapping bot does), with a hive: refund_to (Altera's Hive logins send one):
// nothing is credited to the hive: account on Magi; the BTC goes back to the
// address that SENT it, on Bitcoin, its miner fee taken from it.
func prS2BitcoinSenderRefund(t *testing.T, x *dex3, check prCheck) {
	b := x.b
	d, ctx, cid := b.d, b.ctx, b.v.cid
	start := devnetNow()
	sender, _ := d.bitcoinCli(ctx, "getnewaddress", "", "bech32")
	sender = strings.TrimSpace(sender)
	// The sender's coins: a funding transaction to its own address.
	fundTxid, err := d.bitcoinCli(ctx, "sendtoaddress", sender, "0.00070000")
	if err != nil {
		t.Fatalf("fund sender: %v", err)
	}
	fundTxid = strings.TrimSpace(fundTxid)
	fundH, err := d.MineBlocks(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	fundRaw, _ := d.bitcoinCli(ctx, "getrawtransaction", fundTxid)
	fundRaw = strings.TrimSpace(fundRaw)
	fundVout := ssVout(t, x, fundTxid, sender)
	// The deposit spends exactly that output (its first input) to the swap address.
	instr := "swap_to=hive%3Amagi.test2&swap_asset_out=hbd&destination_chain=HIVE&min_amount_out=999999999999&refund_to=hive%3Amagi.test3"
	gen := &chain.BTCAddressGenerator{Params: &chaincfg.RegressionNetParams, BackupCSVBlocks: 2}
	depAddr, _, err := gen.GenerateDepositAddress(b.v.primary0, backupPubKeyG, instr)
	if err != nil {
		t.Fatalf("deposit address: %v", err)
	}
	const depositSats = 60_000
	rawUnsigned, err := d.bitcoinCli(ctx, "createrawtransaction",
		fmt.Sprintf(`[{"txid":"%s","vout":%d}]`, fundTxid, fundVout), fmt.Sprintf(`{"%s":0.%08d}`, depAddr, depositSats))
	if err != nil {
		t.Fatalf("createrawtransaction: %v", err)
	}
	signed, err := d.bitcoinCli(ctx, "signrawtransactionwithwallet", strings.TrimSpace(rawUnsigned))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	var sres struct {
		Hex      string `json:"hex"`
		Complete bool   `json:"complete"`
	}
	if err := json.Unmarshal([]byte(signed), &sres); err != nil || !sres.Complete {
		t.Fatalf("sign: %v %s", err, signed)
	}
	depTxid, err := d.bitcoinCli(ctx, "sendrawtransaction", sres.Hex)
	if err != nil {
		t.Fatalf("send deposit: %v", err)
	}
	depTxid = strings.TrimSpace(depTxid)
	depH, err := d.MineBlocks(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.MineBlocks(ctx, vfDepositMaturityBlocks); err != nil {
		t.Fatal(err)
	}
	prRelayHeaders(t, x, depH+uint64(vfDepositMaturityBlocks))
	// proveSender, before the map (the mapping bot's order).
	fundTxs := prBlockTxs(t, x, fundH)
	fundIdx := -1
	for i, id := range fundTxs {
		if id == fundTxid {
			fundIdx = i
		}
	}
	if fundIdx < 0 {
		t.Fatalf("funding tx not in block %d", fundH)
	}
	prove := fmt.Sprintf(`{"deposit_tx_hex":"%s","sender":{"block_height":%d,"raw_tx_hex":"%s","merkle_proof_hex":"%s","tx_index":%d}}`,
		sres.Hex, fundH, fundRaw, prMerkleBranch(fundTxs, fundIdx), fundIdx)
	st := vstatus(t, d, ctx, 1, cid, "proveSender", prove)
	check(isOK(st), "S2 proveSender for %s: %s", sender, st)
	check(x.btcState("snd-"+depTxid) == sender, "S2 sender recorded %q, want %s", x.btcState("snd-"+depTxid), sender)
	// The map: the swap cannot meet its minimum.
	depTxs := prBlockTxs(t, x, depH)
	depIdx := -1
	for i, id := range depTxs {
		if id == depTxid {
			depIdx = i
		}
	}
	depRaw, _ := d.bitcoinCli(ctx, "getrawtransaction", depTxid)
	mapPayload := fmt.Sprintf(`{"tx_data":{"block_height":%d,"raw_tx_hex":"%s","merkle_proof_hex":"%s","tx_index":%d},"instructions":["%s"]}`,
		depH, strings.TrimSpace(depRaw), prMerkleBranch(depTxs, depIdx), depIdx, instr)
	st, mapTx := vstatusTx(t, d, ctx, 1, cid, "map", mapPayload)
	check(isOK(st), "S2 map: %s", st)
	t.Logf("S2 map: %s", vfCallResult(d, ctx, 1, mapTx))
	check(x.sats(ssSender) == 0, "S2 nothing credited to the hive: refund_to on Magi (%d sats)", x.sats(ssSender))
	check(x.btcState("rf-"+sender) == strconv.Itoa(depositSats), "S2 held for the sending address %s (%q)", sender, x.btcState("rf-"+sender))
	// Sent home (by a third party: anyone may, as the bot's keeper does).
	recvBefore := prReceived(t, x, sender)
	before := txSpendIds(t, d, ctx, cid)
	st = vstatus(t, d, ctx, 2, cid, "sendHome", `"`+sender+`"`)
	check(isOK(st), "S2 sendHome: %s", st)
	if isOK(st) {
		x.btcCompletePending(before, b.btcKeyID(0))
		got := prReceived(t, x, sender) - recvBefore
		check(got > depositSats-5_000 && got <= depositSats, "S2 USER: %d of %d sats back at the sending address, %s after the deposit", got, depositSats, devnetSince(start).Round(time.Second))
	}
}

// prReceived is what address has received on regtest, in sats.
func prReceived(t *testing.T, x *dex3, address string) int64 {
	t.Helper()
	got, _ := x.b.d.bitcoinCli(x.b.ctx, "getreceivedbyaddress", address, "0")
	f, _ := strconv.ParseFloat(strings.TrimSpace(got), 64)
	return int64(f*1e8 + 0.5)
}

// prS0WireRouter wires the router on the EVM bridge the way mainnet must: a
// timelocked registerRouter proposal. Without it every swap back is refused.
func prS0WireRouter(t *testing.T, x *dex3, check prCheck) {
	e := x.b.e
	if err := e.proposeExecute("registerRouter", x.router); err != nil {
		t.Fatalf("registerRouter on the EVM bridge: %v", err)
	}
	err := x.b.botPoll(10*time.Minute, func() bool { return e.stateStr("routerid") == x.router })
	check(err == nil, "S0 EVM bridge routerid %q, want %s", e.stateStr("routerid"), x.router)

	// GLAM-1: configure the bridge's ETH payout gas limit, which no devnet wave
	// ever did. The bridge signs an ETH payout at the unmap's gas_limit, else the
	// owner-set default (ETHGasLimitKey, an operational proposal), else
	// constants.ETHTransferGas = 21,000. 21,000 pays a transfer to an account
	// that ALREADY EXISTS; under Glamsterdam's state-creation pricing (EIP-8037)
	// a transfer that creates its recipient costs ~207,000 on Sepolia, and this
	// devnet's anvil runs chain id 11155111, for which anvil applies exactly that
	// schedule. So an unconfigured bridge signs every payout to a new address too
	// cheap: it runs out of gas and mines with status 0.
	//
	// That is what failed S10 and S11 of the 2026-11-23 runs, and the L1 dump
	// (prL1VaultTxs) is unambiguous — every value-bearing payout from the vault
	// mined with status 0x0 while both 0-value roll self-transfers, which go to
	// the vault itself (an account that exists), mined with status 0x1:
	//
	//	vault nonce 0x2 to 0xb2310494… value 18354819632724312 status 0x0
	//	vault nonce 0x4 to 0xe9a97960… value 17024137108805723 status 0x0
	//
	// S9 passing in the same suite is the control: it pays the DEPOSITOR back,
	// an address that exists because it sent the deposit, and 21,000 covers it.
	//
	// This is configuration, not a contract bug — but it is configuration a real
	// deployment on any Glamsterdam-priced chain MUST do, or every swap payout to
	// a fresh wallet reverts and loses its fee. It belongs in the deploy order.
	if err := e.proposeExecute("setETHGasLimit", `{"gas_limit":250000}`); err != nil {
		t.Fatalf("setETHGasLimit on the EVM bridge: %v", err)
	}
	err = x.b.botPoll(10*time.Minute, func() bool { return e.stateStr("egl") == "250000" })
	check(err == nil, "S0 EVM bridge ETH payout gas limit %q, want 250000 (GLAM-1)", e.stateStr("egl"))
}

// prHeld lists the held returns (phr-<id> from phrh to phrt).
func prHeld(e *evmBridgeEnv) map[uint64]string {
	out := map[uint64]string{}
	head, _ := strconv.ParseUint(e.stateStr("phrh"), 10, 64)
	tail, _ := strconv.ParseUint(e.stateStr("phrt"), 10, 64)
	for id := head; id < tail; id++ {
		if v := e.stateStr("phr-" + strconv.FormatUint(id, 10)); v != "" {
			out[id] = v
		}
	}
	return out
}

// prS8HivePaidPayoutRefused: a swap paid from Hive (HBD, return_address HIVE)
// whose ETH payout the recipient refuses (the MockUSDC contract takes no ETH):
// the value is held (never on a Magi balance), a stranger cannot trigger the
// swap back, and the bot swaps it back to HBD on the sender's Hive account.
func prS8HivePaidPayoutRefused(t *testing.T, x *dex3, check prCheck) {
	b := x.b
	d, ctx, e := b.d, b.ctx, b.e
	if b.usdc == (ethcommon.Address{}) {
		t.Logf("S8 skipped: no MockUSDC (a recipient that refuses ETH)")
		return
	}
	start := devnetNow()
	hiveBefore := gwL1Hbd(t, d, "magi.test3")
	magiEth := func() string { return strings.SplitN(e.stateStr("a-hive:magi.test3-eth"), "|", 2)[0] }
	magiEthBefore := magiEth() // S5 credited it (a Magi-paid swap's refund)
	tail0, _ := strconv.ParseUint(e.stateStr("phrt"), 10, 64)
	instr := fmt.Sprintf(`{"type":"swap","version":"1.0.0","asset_in":"hbd","asset_out":"eth","amount_in":"2000","min_amount_out":"1","recipient":"%s","destination_chain":"ETH","return_address":{"chain":"HIVE","address":"hive:magi.test3"}}`,
		strings.ToLower(b.usdc.Hex()))
	x.call(x.router, "execute", instr, hbdAllow(2000), 60_000)
	// The payout reverts on Ethereum; the bot's confirmSpend holds it.
	heldID := uint64(0)
	err := b.botPoll(40*time.Minute, func() bool {
		for id := range prHeld(e) {
			if id >= tail0 {
				heldID = id
				return true
			}
		}
		t2, _ := strconv.ParseUint(e.stateStr("phrt"), 10, 64)
		return t2 > tail0 // held and already swapped back
	})
	check(err == nil, "S8 the refused payout came back to the bridge's hold (%s)", devnetSince(start).Round(time.Second))
	if err != nil {
		return
	}
	// A stranger (node 2's account, not a relayer) cannot run the swap back.
	id, cerr := d.CallContract(ctx, 2, e.amID, "retryPayoutReturn", fmt.Sprintf(`{"id":%d,"min_out":"1"}`, heldID))
	ok, msg := false, fmt.Sprint(cerr)
	if cerr == nil {
		ok, msg = e.outResult(ctx, id)
	}
	check(!ok, "S8 a stranger's swap back refused: %s", msg)
	check(magiEth() == magiEthBefore, "S8 nothing put on the sender's Magi ETH balance (%q -> %q)", magiEthBefore, magiEth())
	// The bot swaps it back to HBD and the router pays it on Hive.
	got := int64(0)
	err = b.botPoll(30*time.Minute, func() bool {
		got = gwL1Hbd(t, d, "magi.test3") - hiveBefore
		return got > 0
	})
	check(err == nil && got > 1_500 && got <= 2_000, "S8 USER: %d of 2000 milli-HBD back on Hive, %s after the swap", got, devnetSince(start).Round(time.Second))
	check(len(prHeld(e)) == 0 || prHeld(e)[heldID] == "", "S8 nothing left held (%v)", prHeld(e))
}

// prS9EthereumDepositSwapOwedHome: an ETH deposit asking for a swap that
// cannot meet its minimum: owed home (never on the depositor's Magi balance),
// a stranger cannot send it (or pick its gas), and the bot sends it back to
// the depositor's address, its gas and proving fee taken from it.
func prS9EthereumDepositSwapOwedHome(t *testing.T, x *dex3, check prCheck) {
	b := x.b
	d, ctx, e := b.d, b.ctx, b.e
	start := devnetNow()
	key := mustKey(t)
	addr := ethCrypto.PubkeyToAddress(key.PublicKey)
	did := evmDID(addr)
	if err := b.anvil.SetBalance(ctx, addr, bbEth(1)); err != nil {
		t.Fatal(err)
	}
	deposit := new(big.Int).Div(bbEth(1), big.NewInt(20)) // 0.05 ETH
	data := []byte("swap_to=hive:magi.test2&asset_out=hbd&min_amount_out=999999999999")
	rc, err := sendValue(ctx, b.anvil, key, b.vault0, deposit, data, 100_000)
	if err != nil || rc.Status != 1 {
		t.Fatalf("deposit with a swap: %v", err)
	}
	l1After := b.l1Balance(addr)
	// Mapped by the bot; the swap fails; owed home.
	err = b.botPoll(30*time.Minute, func() bool { return e.stateStr("oh-eth-"+did) != "" || b.l1Balance(addr).Cmp(l1After) > 0 })
	check(err == nil, "S9 owed home to %s (%q)", did, e.stateStr("oh-eth-"+did))
	check(e.stateStr("a-"+did+"-eth") == "" || strings.HasPrefix(e.stateStr("a-"+did+"-eth"), "0"), "S9 nothing on the depositor's Magi balance (%q)", e.stateStr("a-"+did+"-eth"))
	if e.stateStr("oh-eth-"+did) != "" {
		// A stranger cannot send it, nor pick a gas limit that burns it.
		id, cerr := d.CallContract(ctx, 2, e.amID, "sendHome", "address="+strings.ToLower(addr.Hex())+"&asset=eth&owed=1&gas_limit=300000")
		ok, msg := false, fmt.Sprint(cerr)
		if cerr == nil {
			ok, msg = e.outResult(ctx, id)
		}
		check(!ok, "S9 a stranger's owed sendHome refused: %s", msg)
	}
	// The bot sends it home on Ethereum.
	err = b.botPoll(40*time.Minute, func() bool { return b.l1Balance(addr).Cmp(l1After) > 0 })
	back := new(big.Int).Sub(b.l1Balance(addr), l1After)
	lost := new(big.Int).Sub(deposit, back)
	check(err == nil && back.Sign() > 0 && lost.Cmp(new(big.Int).Div(deposit, big.NewInt(10))) < 0,
		"S9 USER: %s of %s wei back at the depositor (%s wei in fees), %s after the deposit", back, deposit, lost, devnetSince(start).Round(time.Second))
}

// prS10RolledPayoutResent: a payout that cannot reach L1 (the bot is down for
// the whole 1,200-block window) is rolled by anyone, and the bot then sends it
// again to the same address: the recipient is paid, less the second send's
// own gas; nothing goes back or to a Magi balance.
func prS10RolledPayoutResent(t *testing.T, x *dex3, check prCheck) {
	b := x.b
	d, ctx, e := b.d, b.ctx, b.e
	start := devnetNow()
	recipient := ethCrypto.PubkeyToAddress(mustKey(t).PublicKey)
	// Only the confirmed-nonce head can be rolled: wait until the bot has
	// confirmed every earlier withdrawal (S9's send home) before it goes down,
	// so this payout is the head (run 4: S9's send was mined but not yet
	// confirmed, the roll was refused and the original payout went out).
	if err := b.botPoll(30*time.Minute, func() bool { return e.stateStr("n") != "" && e.stateStr("n") == e.stateStr("np") }); err != nil {
		check(false, "S10 earlier withdrawals confirmed before the bot goes down (n %q, np %q)", e.stateStr("n"), e.stateStr("np"))
		return
	}
	// The bot is down: the payout is signed but never broadcast.
	b.botMu.Lock()
	b.bot.Stop()
	b.oldLogs = append(b.oldLogs, b.bot.Logs()...)
	b.botMu.Unlock()
	nonce, _ := strconv.ParseUint(e.stateStr("np"), 10, 64)
	instr := fmt.Sprintf(`{"type":"swap","version":"1.0.0","asset_in":"hbd","asset_out":"eth","amount_in":"2000","min_amount_out":"1","recipient":"%s","destination_chain":"ETH"}`,
		strings.ToLower(recipient.Hex()))
	x.call(x.router, "execute", instr, hbdAllow(2000), 60_000)
	var ps struct {
		Amount      json.Number `json:"amount"`
		BlockHeight uint64      `json:"block_height"`
		Payout      bool        `json:"payout"`
	}
	key := "d-" + strconv.FormatUint(nonce, 10)
	err := pollUntil(ctx, evmL2Wait, func() bool { return json.Unmarshal([]byte(e.stateStr(key)), &ps) == nil && ps.Payout })
	check(err == nil, "S10 payout issued at nonce %d (%s)", nonce, e.stateStr(key))
	if err != nil {
		b.restartBotFromStopped()
		return
	}
	first, _ := new(big.Int).SetString(ps.Amount.String(), 10)
	// Past the 1,200-block window (counted on the planted, finalized header), anyone rolls it.
	if err := b.anvil.Mine(ctx, 1300); err != nil { // the 1,200-block window + anvil finality (head - 64)
		t.Fatal(err)
	}
	fin, err := b.anvil.Finalized(ctx)
	if err == nil {
		err = e.plantFinalized(b.anvil, fin)
	}
	check(err == nil, "S10 L1 header past the roll window planted: %v", err)
	id, cerr := d.CallContract(ctx, 2, e.amID, "rollWithdrawal", fmt.Sprintf(`{"nonce":%d}`, nonce))
	ok, msg := false, fmt.Sprint(cerr)
	if cerr == nil {
		ok, msg = e.outResult(ctx, id)
	}
	check(ok, "S10 a third party rolled the stuck payout: %s", msg)
	// The bot comes back: it broadcasts the roll, its confirmSpend sends the
	// payout again (TSS-signed), and it broadcasts that.
	b.restartBotFromStopped()
	before := b.l1Balance(recipient)
	err = b.botPoll(prPayWait(60*time.Minute), func() bool { return b.l1Balance(recipient).Cmp(before) > 0 })
	paid := new(big.Int).Sub(b.l1Balance(recipient), before)
	t.Logf("S10 recipient %s: before %s, now %s", recipient.Hex(), before, b.l1Balance(recipient))
	check(err == nil && paid.Sign() > 0 && paid.Cmp(first) < 0, "S10 USER: the recipient got %s wei (first send was %s; the re-send paid its own gas), %s after the swap", paid, first, devnetSince(start).Round(time.Second))
	check(len(prHeld(e)) == 0, "S10 nothing held (%v)", prHeld(e))
}

// restartBotFromStopped starts a bot after prS10 stopped it.
func (b *bothEnv) restartBotFromStopped() {
	nb := StartBot(b.t, b.ctx, b.botCfg)
	b.botMu.Lock()
	b.bot = nb
	b.botMu.Unlock()
}

// prFundReserve tops the EVM bridge's gas reserve up the way mainnet does (a
// to_reserve deposit): a payout's gas is lent from it (the gap to the signed
// maximum, returned at settlement), so with no reserve every ETH payout waits.
func prFundReserve(t *testing.T, x *dex3) {
	b := x.b
	ctx, e := b.ctx, b.e
	key := mustKey(t)
	if err := b.anvil.SetBalance(ctx, ethCrypto.PubkeyToAddress(key.PublicKey), bbEth(2)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.anvil.SendToReserve(ctx, key, b.vault0, new(big.Int).Div(bbEth(1), big.NewInt(10))); err != nil {
		t.Fatalf("to_reserve top-up: %v", err)
	}
	// The env's L1 loop plants the headers the bot asks for once finalized
	// (anvil finalizes head - 64).
	_ = b.anvil.Mine(ctx, 70)
	want := new(big.Int).Div(bbEth(1), big.NewInt(10))
	if err := b.botPoll(15*time.Minute, func() bool {
		g, ok := new(big.Int).SetString(e.stateStr("gr"), 10)
		return ok && g.Cmp(want) >= 0
	}); err != nil {
		t.Fatalf("the to_reserve top-up was not mapped: gr %q", e.stateStr("gr"))
	}
	t.Logf("gas reserve funded: %s wei", e.stateStr("gr"))
}

// prS11BotRollsAPricedOutPayout (AUTO-ROLL, THORChain's unstuck): a payout
// signed while the bot is down, then Ethereum's base fee held above its fee cap
// past the 1,200-block window. The bot comes back, sees the head cannot mine,
// rolls it itself (nobody else calls rollWithdrawal here), and the payout is
// sent again: the recipient is paid, less the re-send's own gas.
func prS11BotRollsAPricedOutPayout(t *testing.T, x *dex3, check prCheck) {
	b := x.b
	ctx, e := b.ctx, b.e
	start := devnetNow()
	recipient := ethCrypto.PubkeyToAddress(mustKey(t).PublicKey)
	if err := b.botPoll(30*time.Minute, func() bool { return e.stateStr("n") != "" && e.stateStr("n") == e.stateStr("np") }); err != nil {
		check(false, "S11 earlier withdrawals confirmed before the bot goes down (n %q, np %q)", e.stateStr("n"), e.stateStr("np"))
		return
	}
	b.botMu.Lock()
	b.bot.Stop()
	b.oldLogs = append(b.oldLogs, b.bot.Logs()...)
	b.botMu.Unlock()
	nonce, _ := strconv.ParseUint(e.stateStr("np"), 10, 64)
	instr := fmt.Sprintf(`{"type":"swap","version":"1.0.0","asset_in":"hbd","asset_out":"eth","amount_in":"2000","min_amount_out":"1","recipient":"%s","destination_chain":"ETH"}`,
		strings.ToLower(recipient.Hex()))
	x.call(x.router, "execute", instr, hbdAllow(2000), 60_000)
	key := "d-" + strconv.FormatUint(nonce, 10)
	var ps struct {
		Amount json.Number `json:"amount"`
		Payout bool        `json:"payout"`
		Rolled bool        `json:"rolled"`
	}
	read := func() bool { return json.Unmarshal([]byte(e.stateStr(key)), &ps) == nil }
	err := pollUntil(ctx, evmL2Wait, func() bool { return read() && ps.Payout })
	check(err == nil, "S11 payout issued at nonce %d (%s)", nonce, e.stateStr(key))
	if err != nil {
		b.restartBotFromStopped()
		return
	}
	first, _ := new(big.Int).SetString(ps.Amount.String(), 10)
	// Past the window (the 1,200 blocks + anvil finality), with the header planted.
	if err := b.anvil.Mine(ctx, 1300); err != nil {
		t.Fatal(err)
	}
	fin, err := b.anvil.Finalized(ctx)
	if err == nil {
		err = e.plantFinalized(b.anvil, fin)
	}
	check(err == nil, "S11 L1 header past the roll window planted: %v", err)
	// A gas spike that outlasts the window: the base fee held at 60 gwei, above
	// the payout's ~2 gwei fee cap, until the bot has rolled it.
	spikeCtx, stopSpike := context.WithCancel(ctx)
	defer stopSpike()
	go func() {
		spike := big.NewInt(60e9)
		for spikeCtx.Err() == nil {
			_ = b.anvil.rpc.CallContext(spikeCtx, nil, "anvil_setNextBlockBaseFeePerGas", "0x"+spike.Text(16))
			devnetSleep(500 * time.Millisecond)
		}
	}()
	before := b.l1Balance(recipient)
	b.restartBotFromStopped()
	err = b.botPoll(20*time.Minute, func() bool { return read() && ps.Rolled })
	check(err == nil, "S11 the bot rolled the priced-out payout itself (%s after the swap)", devnetSince(start).Round(time.Second))
	stopSpike()
	if err != nil {
		return
	}
	err = b.botPoll(prPayWait(40*time.Minute), func() bool { return b.l1Balance(recipient).Cmp(before) > 0 })
	paid := new(big.Int).Sub(b.l1Balance(recipient), before)
	t.Logf("S11 recipient %s: before %s, now %s", recipient.Hex(), before, b.l1Balance(recipient))
	check(err == nil && paid.Sign() > 0 && paid.Cmp(first) < 0, "S11 USER: the recipient got %s wei after the bot's roll (first send %s; the re-send paid its own gas at the spiked price), %s after the swap", paid, first, devnetSince(start).Round(time.Second))
	check(len(prHeld(e)) == 0, "S11 nothing held (%v)", prHeld(e))
}
