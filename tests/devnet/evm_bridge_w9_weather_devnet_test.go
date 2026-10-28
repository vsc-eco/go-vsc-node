//go:build evm_devnet

// W9: the bridge in mainnet weather, through the real bot. The bot reaches L1
// only through two flaky RPCs (ChaosRPC: 429s, 502s, dropped connections, lost
// answers, slow answers, a lagging head), as a mainnet bot on free providers
// does, and meets, in order:
//
//	busy block  one block of ~250 txs holding 3 ETH deposits, a USDC deposit
//	            and dust below the floors (TICKSTALL-1, DUST-1)
//	fee spike   the L1 base fee held above the withdrawals' fee cap while two
//	            are in flight, and the bot killed and restarted meanwhile
//	reorg       a mined withdrawal dropped by a shallow L1 reorg before it is
//	            final
//
// and must end with every deposit credited once, dust not credited, every
// withdrawal paid once and confirmed, and no custody alarm.
//
// Run: go test -tags evm_devnet -run TestEVMBridge_W9 ./tests/devnet/ -timeout 180m -v
package devnet

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"vsc-node/lib/dids"

	ethcommon "github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
)

// w9SignedTx signs a tx without sending it.
func w9SignedTx(a *Anvil, key *ecdsa.PrivateKey, nonce uint64, to ethcommon.Address, value *big.Int, data []byte, gas uint64) (*ethtypes.Transaction, error) {
	return ethtypes.SignTx(ethtypes.NewTx(&ethtypes.DynamicFeeTx{ChainID: a.ChainID, Nonce: nonce, GasTipCap: big.NewInt(1e9),
		GasFeeCap: big.NewInt(50e9), Gas: gas, To: &to, Value: value, Data: data}), ethtypes.LatestSignerForChainID(a.ChainID), key)
}

// pendingSpendFeeCap is the max fee per gas of the vault tx the bridge queued
// for nonce n (its d-{n} record's unsigned EIP-1559 tx).
func (e *evmBridgeEnv) pendingSpendFeeCap(n uint64) (*big.Int, error) {
	var ps struct {
		UnsignedTxHex string `json:"unsigned_tx_hex"`
	}
	if err := json.Unmarshal([]byte(e.stateStr(fmt.Sprintf("d-%d", n))), &ps); err != nil {
		return nil, fmt.Errorf("d-%d: %w", n, err)
	}
	raw, err := hex.DecodeString(strings.TrimPrefix(ps.UnsignedTxHex, "0x"))
	if err != nil || len(raw) < 2 || raw[0] != ethtypes.DynamicFeeTxType {
		return nil, fmt.Errorf("d-%d: not an EIP-1559 tx (%v)", n, err)
	}
	var fields []rlp.RawValue
	if err := rlp.DecodeBytes(raw[1:], &fields); err != nil || len(fields) < 4 {
		return nil, fmt.Errorf("d-%d: %v", n, err)
	}
	fee := new(big.Int)
	if err := rlp.DecodeBytes(fields[3], fee); err != nil {
		return nil, err
	}
	return fee, nil
}

func TestEVMBridge_W9_MainnetWeather(t *testing.T) {
	if testing.Short() {
		t.Skip("devnet")
	}
	t.Parallel()
	ctx, cancel := devnetWithTimeout(context.Background(), 170*time.Minute)
	t.Cleanup(cancel)

	e := setupEvmBridgeDevnet(t, ctx)
	anvil := StartAnvil(t, ctx, evmChainID, nextAnvilPort())
	t.Cleanup(anvil.Stop)
	eth := func(n int64, d int64) *big.Int { // n / d ETH
		return new(big.Int).Div(new(big.Int).Mul(big.NewInt(n), big.NewInt(1e18)), big.NewInt(d))
	}
	usd := func(units int64) *big.Int { return big.NewInt(units) }
	anvilCall := func(method string, args ...any) error { return anvil.rpc.CallContext(ctx, nil, method, args...) }

	// Two free providers on a bad day; the bot fails over between them.
	rpcA, err := StartChaosRPC(anvil.URL, MainnetWeather(91))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rpcA.Stop)
	rpcB, err := StartChaosRPC(anvil.URL, MainnetWeather(92))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rpcB.Stop)

	botKey, _ := ethCrypto.GenerateKey()
	botDID := dids.NewEthDID(ethCrypto.PubkeyToAddress(botKey.PublicKey).Hex()).String()
	if err := e.fundDID(botDID, "300.000"); err != nil {
		t.Fatalf("fund bot DID for RC: %v", err)
	}
	ownerKey := mustKey(t)
	if err := anvil.SetBalance(ctx, cryptoPubkeyAddr(ownerKey), eth(10, 1)); err != nil {
		t.Fatal(err)
	}
	usdc, err := anvil.DeployMockUSDC(ctx, ownerKey)
	if err != nil {
		t.Fatalf("deploy MockUSDC: %v", err)
	}
	if err := e.proposeExecute("registerToken", fmt.Sprintf(`{"address":"%s","symbol":"usdc","decimals":6,"min_withdrawal":1000000}`, usdc.Hex())); err != nil {
		t.Fatalf("registerToken usdc: %v", err)
	}
	e.botAttests = true
	vault, err := e.tssVault(25 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := anvil.SetBalance(ctx, vault, eth(10, 1)); err != nil {
		t.Fatalf("fund vault gas: %v", err)
	}
	port, err := freeLocalPort()
	if err != nil {
		t.Fatal(err)
	}
	healthAddr := "127.0.0.1:" + port
	botCfg := BotConfig{
		EthRPC: rpcA.URL + "," + rpcB.URL, ContractID: e.amID, Tokens: usdc.Hex(),
		GraphQLURL: e.d.GQLEndpoint(e.gqlNode), EthPrivHex: hex.EncodeToString(ethCrypto.FromECDSA(botKey)),
		Checkpoint: filepath.Join(t.TempDir(), "w9-checkpoint.json"), HealthAddr: healthAddr, RcLimit: "10000",
	}
	bot := StartBot(t, ctx, botCfg)
	t.Cleanup(func() { bot.Stop() })
	var restarts atomic.Int32
	allLogs := []string{}
	botLogs := func() string { return strings.Join(append(append([]string{}, allLogs...), bot.Logs()...), "\n") }
	if !bot.WaitForLog(ctx, "E23: attested the vault key's check signature", evmL2Wait+15*time.Minute) {
		t.Fatalf("the bot never attested the vault key's check signature (through the flaky RPCs):\n%s", botLogs())
	}
	if !bot.WaitForLog(ctx, "A18: vault is the TSS key's address", evmL2Wait) {
		t.Fatalf("the bot did not take the vault:\n%s", botLogs())
	}
	t.Logf("(0) PASS: vault %s live; the bot reaches L1 only through two flaky RPCs (%s | %s)", vault.Hex(), rpcA, rpcB)

	// (1) A busy block: ~250 txs, among them deposits and dust.
	keys := func(n int, wei *big.Int) []*ecdsa.PrivateKey {
		out := make([]*ecdsa.PrivateKey, n)
		for i := range out {
			out[i] = mustKey(t)
			if err := anvil.SetBalance(ctx, cryptoPubkeyAddr(out[i]), wei); err != nil {
				t.Fatal(err)
			}
		}
		return out
	}
	fillers := keys(8, eth(1, 1))
	deps := keys(3, eth(2, 1))
	dusts := keys(2, eth(1, 100))
	usdcHolder, usdcDust := mustKey(t), mustKey(t)
	for _, k := range []*ecdsa.PrivateKey{usdcHolder, usdcDust} {
		if err := anvil.SetBalance(ctx, cryptoPubkeyAddr(k), eth(1, 1)); err != nil {
			t.Fatal(err)
		}
	}
	for k, amt := range map[*ecdsa.PrivateKey]*big.Int{usdcHolder: usd(10_000_000), usdcDust: usd(1_000_000)} {
		if rc, err := anvil.SendTx(ctx, ownerKey, &usdc, usdcCall("mint(address,uint256)", cryptoPubkeyAddr(k), amt), 100_000); err != nil || rc.Status != 1 {
			t.Fatalf("mint: %v", err)
		}
	}
	depAmounts := []*big.Int{eth(1, 1), eth(1, 2), eth(2, 1000)} // 1, 0.5, 0.002 ETH (the floor is 0.001)
	dustAmounts := []*big.Int{eth(9, 10000), big.NewInt(1)}      // 0.0009 ETH, 1 wei
	if err := anvilCall("evm_setIntervalMining", 0); err != nil {
		t.Fatalf("pause mining: %v", err)
	}
	var txs []*ethtypes.Transaction
	send := func(key *ecdsa.PrivateKey, nonce uint64, to ethcommon.Address, value *big.Int, data []byte, gas uint64) {
		tx, err := w9SignedTx(anvil, key, nonce, to, value, data, gas)
		if err == nil {
			err = anvil.cli.SendTransaction(ctx, tx)
		}
		if err != nil {
			t.Fatalf("send: %v", err)
		}
		txs = append(txs, tx)
	}
	for _, k := range fillers {
		for n := uint64(0); n < 30; n++ {
			send(k, n, cryptoPubkeyAddr(mustKey(t)), big.NewInt(1e9), nil, 21_000)
		}
	}
	for i, k := range deps {
		send(k, 0, vault, depAmounts[i], nil, 21_000)
	}
	for i, k := range dusts {
		send(k, 0, vault, dustAmounts[i], nil, 21_000)
	}
	usdcNonce := func(k *ecdsa.PrivateKey) uint64 {
		n, err := anvil.cli.PendingNonceAt(ctx, cryptoPubkeyAddr(k))
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	send(usdcHolder, usdcNonce(usdcHolder), usdc, new(big.Int), usdcCall("transfer(address,uint256)", vault, usd(5_000_000)), 100_000)
	send(usdcDust, usdcNonce(usdcDust), usdc, new(big.Int), usdcCall("transfer(address,uint256)", vault, usd(50_000)), 100_000) // 0.05 USDC; the floor is 0.1
	if err := anvil.Mine(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := anvilCall("evm_setIntervalMining", 1); err != nil {
		t.Fatalf("resume mining: %v", err)
	}
	var busy uint64
	for _, tx := range txs {
		rc, err := anvil.waitReceipt(ctx, tx.Hash(), 30*time.Second)
		if err != nil || rc.Status != 1 {
			t.Fatalf("busy block tx %s: %v %+v", tx.Hash().Hex(), err, rc)
		}
		if busy == 0 {
			busy = rc.BlockNumber.Uint64()
		} else if rc.BlockNumber.Uint64() != busy {
			t.Fatalf("the busy block split: %s in block %d, others in %d", tx.Hash().Hex(), rc.BlockNumber.Uint64(), busy)
		}
	}
	if err := e.plantFinalized(anvil, busy); err != nil {
		t.Fatal(err)
	}
	t.Logf("(1) block %d: %d txs (240 fillers, 3 ETH deposits, 2 dust ETH, 1 USDC deposit, 1 dust USDC)", busy, len(txs))
	want := map[string]string{}
	for i, k := range deps {
		credit := new(big.Int).Div(new(big.Int).Mul(depAmounts[i], big.NewInt(99)), big.NewInt(100)) // 1% to the gas reserve
		want["a-"+evmDID(cryptoPubkeyAddr(k))+"-eth"] = credit.String()
	}
	want["a-"+evmDID(cryptoPubkeyAddr(usdcHolder))+"-usdc"] = "5000000"
	credited := func() bool {
		for k, v := range want {
			if e.stateStr(k) != v {
				return false
			}
		}
		return true
	}
	if err := pollUntil(ctx, 15*time.Minute, credited); err != nil {
		got := map[string]string{}
		for k := range want {
			got[k] = e.stateStr(k)
		}
		t.Fatalf("TICKSTALL-1: the busy block's deposits were not all credited: got %v want %v\nbot:\n%s", got, want, botLogs())
	}
	devnetSleep(60 * time.Second) // a dust map, if the bot sent one, would have landed by now
	for _, k := range dusts {
		if got := e.stateStr("a-" + evmDID(cryptoPubkeyAddr(k)) + "-eth"); got != "" {
			t.Fatalf("DUST-1: an ETH deposit below the floor was credited: %s", got)
		}
	}
	if got := e.stateStr("a-" + evmDID(cryptoPubkeyAddr(usdcDust)) + "-usdc"); got != "" {
		t.Fatalf("DUST-1: a USDC deposit below the floor was credited: %s", got)
	}
	t.Logf("(1) PASS: every deposit in the busy block credited once, dust not credited (RPCs so far: %s | %s)", rpcA, rpcB)

	// (2) Two withdrawals while the L1 base fee spikes above their fee cap; the
	// bot is killed and restarted meanwhile.
	for _, k := range deps[:2] {
		if err := e.fundDID(evmDID(cryptoPubkeyAddr(k)), "100.000"); err != nil {
			t.Fatal(err)
		}
	}
	dK := func(k *ecdsa.PrivateKey) *EthKey {
		ek, _ := NewEthKey(hex.EncodeToString(ethCrypto.FromECDSA(k)))
		return ek
	}
	to1, to2, to3 := cryptoPubkeyAddr(mustKey(t)), cryptoPubkeyAddr(mustKey(t)), cryptoPubkeyAddr(mustKey(t))
	nBefore := e.stateStr("np")
	from2, err := anvil.cli.BlockNumber(ctx) // the withdrawals mine after this
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.d.CallContractEvm(ctx, e.gqlNode, dK(deps[0]), e.amID, "unmapETH", fmt.Sprintf(`{"to":"%s","amount":"300000000000000000","asset":"eth"}`, to1.Hex()), w8UnmapRC); err != nil {
		t.Fatalf("unmapETH 1: %v", err)
	}
	if _, err := e.d.CallContractEvm(ctx, e.gqlNode, dK(deps[1]), e.amID, "unmapETH", fmt.Sprintf(`{"to":"%s","amount":"200000000000000000","asset":"eth"}`, to2.Hex()), w8UnmapRC); err != nil {
		t.Fatalf("unmapETH 2: %v", err)
	}
	if err := pollUntil(ctx, evmL2Wait, func() bool { return e.stateStr("d-0") != "" && e.stateStr("d-1") != "" }); err != nil {
		t.Fatalf("the two withdrawals were not issued (np %q -> %q)", nBefore, e.stateStr("np"))
	}
	fee0, err := e.pendingSpendFeeCap(0)
	if err != nil {
		t.Fatal(err)
	}
	fee1, err := e.pendingSpendFeeCap(1)
	if err != nil {
		t.Fatal(err)
	}
	spike := new(big.Int).Mul(fee0, big.NewInt(3))
	if fee1.Cmp(fee0) > 0 {
		spike = new(big.Int).Mul(fee1, big.NewInt(3))
	}
	if min := big.NewInt(50e9); spike.Cmp(min) < 0 {
		spike = min
	}
	sctx, stopSpike := context.WithCancel(ctx)
	spikeDone := make(chan struct{})
	go func() { // hold the next block's base fee at the spike
		defer close(spikeDone)
		for sctx.Err() == nil {
			_ = anvil.rpc.CallContext(sctx, nil, "anvil_setNextBlockBaseFeePerGas", "0x"+spike.Text(16))
			select {
			case <-sctx.Done():
			case <-devnetAfter(400 * time.Millisecond):
			}
		}
	}()
	t.Logf("(2) fee caps %s / %s wei; L1 base fee held at %s wei", fee0, fee1, spike)
	attempted := func(lines []string) bool {
		for _, l := range lines {
			if strings.Contains(l, "withdrawal TX broadcast to Ethereum") || strings.Contains(l, "broadcast failed with both v values") {
				return true
			}
		}
		return false
	}
	if err := pollUntil(ctx, 20*time.Minute, func() bool { return attempted(bot.Logs()) }); err != nil {
		stopSpike()
		t.Fatalf("the bot never tried to broadcast the withdrawals:\n%s", botLogs())
	}
	allLogs = append(allLogs, bot.Logs()...)
	bot.Stop() // a crash mid-flight
	restarts.Add(1)
	devnetSleep(5 * time.Second)
	bot = StartBot(t, ctx, botCfg)
	devnetSleep(3 * time.Minute) // the restarted bot meets the spike too
	for n := uint64(0); n < 2; n++ {
		if _, b, found, err := anvil.FindTxFrom(ctx, vault, n, from2); err == nil && found {
			stopSpike()
			t.Fatalf("withdrawal %d mined in block %d while the base fee was above its fee cap: the spike did not hold", n, b)
		}
	}
	stopSpike()
	<-spikeDone
	t.Logf("(2) the spike held both withdrawals back, and the bot was restarted under it; base fee released")
	mined := map[uint64]uint64{}
	if err := pollUntil(ctx, 20*time.Minute, func() bool {
		for n := uint64(0); n < 2; n++ {
			if _, ok := mined[n]; ok {
				continue
			}
			if _, b, found, ferr := anvil.FindTxFrom(ctx, vault, n, from2); ferr == nil && found {
				mined[n] = b
			}
		}
		return len(mined) == 2
	}); err != nil {
		t.Fatalf("after the spike the vault did not send both withdrawals (got %v)\nbot:\n%s", mined, botLogs())
	}
	for _, b := range mined {
		if err := e.plantFinalized(anvil, b); err != nil {
			t.Fatal(err)
		}
	}
	if err := pollUntil(ctx, 15*time.Minute, func() bool { return e.stateStr("n") == "2" }); err != nil {
		t.Fatalf("the bot did not confirm both withdrawals after the spike: n=%q\nbot:\n%s", e.stateStr("n"), botLogs())
	}
	t.Logf("(2) PASS: both withdrawals mined once the spike passed and were confirmed (n=2), across a bot restart")

	// (3) A shallow reorg drops a mined withdrawal before it is final.
	from3, err := anvil.cli.BlockNumber(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.d.CallContractEvm(ctx, e.gqlNode, dK(deps[0]), e.amID, "unmapETH", fmt.Sprintf(`{"to":"%s","amount":"100000000000000000","asset":"eth"}`, to3.Hex()), w8UnmapRC); err != nil {
		t.Fatalf("unmapETH 3: %v", err)
	}
	var first uint64
	if err := pollUntil(ctx, 20*time.Minute, func() bool {
		_, b, found, ferr := anvil.FindTxFrom(ctx, vault, 2, from3)
		first = b
		return ferr == nil && found
	}); err != nil {
		t.Fatalf("withdrawal 2 never mined:\n%s", botLogs())
	}
	head, err := anvil.cli.BlockNumber(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if f, err := anvil.Finalized(ctx); err != nil || f >= first {
		t.Fatalf("withdrawal 2's block %d is already final (%d): no shallow reorg possible", first, f)
	}
	depth := head - first + 1
	if err := anvilCall("anvil_reorg", depth, []any{}); err != nil {
		t.Fatalf("anvil_reorg %d: %v", depth, err)
	}
	_, _, again, _ := anvil.FindTxFrom(ctx, vault, 2, from3)
	t.Logf("(3) reorged %d blocks (withdrawal 2 had mined in %d); right after the reorg it is on chain: %v", depth, first, again)
	var final uint64
	if err := pollUntil(ctx, 15*time.Minute, func() bool {
		_, b, found, ferr := anvil.FindTxFrom(ctx, vault, 2, from3)
		final = b
		return ferr == nil && found
	}); err != nil {
		t.Fatalf("after the reorg withdrawal 2 was never mined again (the bot must re-post it):\n%s", botLogs())
	}
	if err := e.plantFinalized(anvil, final); err != nil {
		t.Fatal(err)
	}
	if err := pollUntil(ctx, 15*time.Minute, func() bool { return e.stateStr("n") == "3" }); err != nil {
		t.Fatalf("withdrawal 2 was not confirmed after the reorg: n=%q\nbot:\n%s", e.stateStr("n"), botLogs())
	}
	t.Logf("(3) PASS: the reorged withdrawal was mined again (block %d, first %d) and confirmed once (n=3)", final, first)

	// (4) The end state.
	for to, amt := range map[ethcommon.Address]*big.Int{to1: eth(3, 10), to2: eth(2, 10), to3: eth(1, 10)} {
		bal, err := anvil.cli.BalanceAt(ctx, to, nil)
		if err != nil || bal.Cmp(amt) != 0 {
			t.Fatalf("recipient %s holds %v, want exactly %s (paid once)", to.Hex(), bal, amt)
		}
	}
	if np, n := e.stateStr("np"), e.stateStr("n"); np != n || e.stateStr("wqh") != e.stateStr("wqt") {
		t.Fatalf("withdrawals left: np=%s n=%s queue %s..%s", np, n, e.stateStr("wqh"), e.stateStr("wqt"))
	}
	for _, l := range strings.Split(botLogs(), "\n") {
		if strings.Contains(l, "panic") || strings.Contains(strings.ToLower(l), "dead-letter") || strings.Contains(l, "dead_letter") {
			t.Fatalf("bot log: %s", l)
		}
	}
	var h *BotHealth
	if err := pollUntil(ctx, 3*time.Minute, func() bool {
		var herr error
		h, herr = bot.Health(ctx, healthAddr)
		return herr == nil && h.CheckedAt != ""
	}); err != nil {
		t.Fatalf("no /health from the restarted bot")
	}
	for _, i := range h.Issues {
		if strings.HasPrefix(i, "C4") {
			t.Fatalf("custody alarm at the end: %v", h.Issues)
		}
	}
	a, b := rpcA.Counts(), rpcB.Counts()
	sum := func(k string) int { return a[k] + b[k] }
	t.Logf("(4) RPC weather met: A %s | B %s; bot restarts %d; /health %s %v", rpcA, rpcB, restarts.Load(), h.Status, h.Issues)
	for k, min := range map[string]int{"calls": 500, "429": 10, "502": 2, "drop": 1, "lost": 1, "slow": 10, "stale": 5} {
		if sum(k) < min {
			t.Fatalf("the bot met too little weather to show anything: %s=%d (want >= %d)", k, sum(k), min)
		}
	}
	t.Logf("(4) PASS: every deposit credited once, dust not, every withdrawal paid once and confirmed, queue empty, no custody alarm, no dead letter")
}
