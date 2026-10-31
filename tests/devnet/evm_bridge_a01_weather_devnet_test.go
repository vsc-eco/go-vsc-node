//go:build evm_devnet

// A01 under mainnet weather: the rotation of TestEVMBridge_A01_VaultRotation
// while the network and L1 misbehave, as they do on mainnet. The bot reads L1
// through two flaky free-RPC stand-ins (ChaosRPC: 429s, dropped calls, lost
// answers, lagging heads); a witness is down during generation 1's keygen and
// another during its reshare; an L1 fee spike holds the batch back and the bot
// is restarted under it; a shallow reorg drops the mined ETH sweep; the
// prover delivers the sweeps' headers late. It logs how long each fault held
// things up and the user window, and checks the end state, not only that the
// steps ran.
//
//	eval "$(tests/devnet/suite/evm-artifacts.sh)"
//	go test -tags evm_devnet -run TestEVMBridge_A01_Weather ./tests/devnet/ -timeout 210m -v
package devnet

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vsc-node/lib/dids"

	ethcommon "github.com/ethereum/go-ethereum/common"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

func TestEVMBridge_A01_Weather(t *testing.T) {
	if testing.Short() {
		t.Skip("devnet")
	}
	ctx, e := a01Devnet(t, 200*time.Minute)
	anvil := StartAnvil(t, ctx, evmChainID, nextAnvilPort())
	t.Cleanup(anvil.Stop)
	eth := func(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(1e18)) }
	usd := func(n int64) *big.Int { return big.NewInt(n * 1_000_000) }
	state := func(k string) string { return e.stateStr(k) }
	timings := map[string]time.Duration{}

	rpcA, err := StartChaosRPC(anvil.URL, MainnetWeather(131))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rpcA.Stop)
	rpcB, err := StartChaosRPC(anvil.URL, MainnetWeather(132))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rpcB.Stop)

	ownerKey := mustKey(t)
	if err := anvil.SetBalance(ctx, cryptoPubkeyAddr(ownerKey), eth(100)); err != nil {
		t.Fatal(err)
	}
	usdc, err := anvil.DeployMockUSDC(ctx, ownerKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.proposeExecute("registerToken", fmt.Sprintf(`{"address":"%s","symbol":"usdc","decimals":6,"min_withdrawal":1000000}`, usdc.Hex())); err != nil {
		t.Fatalf("registerToken: %v", err)
	}
	botKey := mustKey(t)
	botDID := dids.NewEthDID(ethCrypto.PubkeyToAddress(botKey.PublicKey).Hex()).String()
	if err := e.proposeExecute("registerRelayer", fmt.Sprintf(`{"account":"%s"}`, botDID)); err != nil {
		t.Fatalf("registerRelayer: %v", err)
	}
	if err := e.fundDID(botDID, "50000.000"); err != nil {
		t.Fatalf("fund bot DID: %v", err)
	}
	e.botAttests = true
	vault0, err := e.tssVault(25 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := anvil.SetBalance(ctx, vault0, eth(1)); err != nil {
		t.Fatal(err)
	}
	botCfg := BotConfig{
		EthRPC: rpcA.URL + "," + rpcB.URL, ContractID: e.amID, GraphQLURL: e.d.GQLEndpoint(e.gqlNode),
		EthPrivHex: hex.EncodeToString(ethCrypto.FromECDSA(botKey)), Tokens: usdc.Hex(),
		Checkpoint: filepath.Join(t.TempDir(), "a01w-checkpoint.json"),
	}
	bot := StartBot(t, ctx, botCfg)
	t.Cleanup(func() { bot.Stop() })
	var earlier []string
	botLogs := func() string { return strings.Join(append(append([]string{}, earlier...), bot.Logs()...), "\n") }
	if !bot.WaitForLog(ctx, "A18: vault is the TSS key's address", evmL2Wait+15*time.Minute) {
		t.Fatalf("the bot did not take the vault:\n%s", botLogs())
	}
	if ok, msg := e.ownerCall("setVaultOperator", botDID); !ok {
		t.Fatalf("setVaultOperator: %s", msg)
	}

	// Deposits: 6 ETH and 25 USDC to generation 0.
	depKey := mustKey(t)
	depAddr := ethCrypto.PubkeyToAddress(depKey.PublicKey)
	user := evmDID(depAddr)
	if err := anvil.SetBalance(ctx, depAddr, eth(8)); err != nil {
		t.Fatal(err)
	}
	_, ethBlock, err := anvil.SendETH(ctx, depKey, vault0, eth(6))
	if err != nil {
		t.Fatal(err)
	}
	if rc, err := anvil.SendTx(ctx, ownerKey, &usdc, usdcCall("mint(address,uint256)", depAddr, usd(30)), 100_000); err != nil || rc.Status != 1 {
		t.Fatalf("mint: %v", err)
	}
	rc, err := anvil.SendTx(ctx, depKey, &usdc, usdcCall("transfer(address,uint256)", vault0, usd(25)), 100_000)
	if err != nil || rc.Status != 1 {
		t.Fatalf("usdc deposit: %v", err)
	}
	for _, b := range []uint64{ethBlock, rc.BlockNumber.Uint64()} {
		if err := e.plantFinalized(anvil, b); err != nil {
			t.Fatal(err)
		}
	}
	if err := pollUntil(ctx, 12*time.Minute, func() bool {
		return state("a-"+user+"-eth") == "5940000000000000000" && state("a-"+user+"-usdc") == "25000000"
	}); err != nil {
		t.Fatalf("deposits not mapped through the flaky RPCs: eth %q usdc %q\nbot:\n%s", state("a-"+user+"-eth"), state("a-"+user+"-usdc"), botLogs())
	}
	if err := e.fundDID(user, "10000.000"); err != nil {
		t.Fatal(err)
	}
	K, err := NewEthKey(hex.EncodeToString(ethCrypto.FromECDSA(depKey)))
	if err != nil {
		t.Fatal(err)
	}
	unmap := func(to ethcommon.Address, wei string) {
		t.Helper()
		if _, err := e.d.CallContractEvm(ctx, e.gqlNode, K, e.amID, "unmapETH", fmt.Sprintf(`{"to":"%s","amount":"%s","asset":"eth"}`, to.Hex(), wei), 5000000); err != nil {
			t.Fatalf("unmapETH: %v", err)
		}
	}
	t.Logf("(W0) deposits mapped through two flaky RPCs")

	// A generation-0 withdrawal in flight at the cut-over.
	unmap(ethCrypto.PubkeyToAddress(mustKey(t).PublicKey), "1000000000000000000")
	var w1Block uint64
	if err := pollUntil(ctx, 20*time.Minute, func() bool {
		_, n, found, ferr := anvil.FindTxFrom(ctx, vault0, 0, ethBlock)
		w1Block = n
		return ferr == nil && found
	}); err != nil {
		t.Fatalf("generation 0's withdrawal never reached L1\nbot:\n%s", botLogs())
	}

	// Flaky witnesses: one down through generation 1's keygen, another
	// through its check signature and reshare.
	flaky := []int{}
	for n := 2; n <= 4 && len(flaky) < 2; n++ {
		if n != e.gqlNode && n != e.ownerNode {
			flaky = append(flaky, n)
		}
	}
	ceremony := devnetNow()
	if ok, msg := e.ownerCall("createKey", "{}"); !ok {
		t.Fatalf("createKey: %s", msg)
	}
	if err := e.d.StopNode(ctx, flaky[0]); err != nil {
		t.Fatal(err)
	}
	devnetSleep(4 * time.Minute)
	if err := e.d.StartNode(ctx, flaky[0]); err != nil {
		t.Fatal(err)
	}
	if err := pollUntil(ctx, 30*time.Minute, func() bool { return e.keyPub("primaryv1") != "" }); err != nil {
		t.Fatal("generation 1's keygen never landed after the witness outage")
	}
	timings["createKey -> keygen (magi-"+fmt.Sprint(flaky[0])+" down 4 min)"] = devnetSince(ceremony)
	if err := e.d.StopNode(ctx, flaky[1]); err != nil {
		t.Fatal(err)
	}
	restartAt := devnetNow().Add(4 * time.Minute)
	var t0 time.Time
	var last string
	if err := pollUntil(ctx, 50*time.Minute, func() bool {
		if !restartAt.IsZero() && devnetNow().After(restartAt) {
			if err := e.d.StartNode(ctx, flaky[1]); err == nil {
				restartAt = time.Time{}
			}
		}
		ok, msg := e.ownerCall("activateKey", "{}")
		last = msg
		if ok {
			t0 = devnetNow()
		} else {
			devnetSleep(45 * time.Second)
		}
		return ok
	}); err != nil {
		t.Fatalf("activateKey never succeeded after the witness outages; last: %s", last)
	}
	if !restartAt.IsZero() {
		_ = e.d.StartNode(ctx, flaky[1])
	}
	timings["createKey -> activateKey (old generation serving throughout)"] = devnetSince(ceremony)
	vault1, ok := tssKeyAddress(e.keyPub("primaryv1"))
	if !ok {
		t.Fatal("generation 1 key is not secp256k1")
	}
	t.Logf("(W1) cut over to %s after two witness outages", vault1.Hex())

	// A withdrawal after the cut-over waits; generation 0's settles.
	recip2 := ethCrypto.PubkeyToAddress(mustKey(t).PublicKey)
	unmap(recip2, "500000000000000000")
	if err := e.plantFinalized(anvil, w1Block); err != nil {
		t.Fatal(err)
	}

	// The batch, then a fee spike above the sweeps' fee caps, and the bot
	// restarted under it.
	if err := pollUntil(ctx, 15*time.Minute, func() bool { return strings.Contains(state("msr"), "0-1") && strings.Contains(state("msr"), "0-2") }); err != nil {
		t.Fatalf("no batch: msr=%q\nbot:\n%s", state("msr"), botLogs())
	}
	batchAt := devnetNow()
	spike := big.NewInt(60e9) // far above the 2x base fee + 2 gwei the sweeps were priced at
	sctx, stopSpike := context.WithCancel(ctx)
	spikeDone := make(chan struct{})
	go func() {
		defer close(spikeDone)
		for sctx.Err() == nil {
			_ = anvil.rpc.CallContext(sctx, nil, "anvil_setNextBlockBaseFeePerGas", "0x"+spike.Text(16))
			select {
			case <-sctx.Done():
			case <-devnetAfter(400 * time.Millisecond):
			}
		}
	}()
	devnetSleep(2 * time.Minute)
	bot.Stop()
	devnetSleep(5 * time.Second)
	earlier = append(earlier, bot.Logs()...)
	bot = StartBot(t, ctx, botCfg)
	devnetSleep(2 * time.Minute)
	if _, b, found, _ := anvil.FindTxFrom(ctx, vault0, 1, w1Block); found {
		t.Fatalf("a sweep mined in block %d under the spike: the spike did not hold", b)
	}
	stopSpike()
	<-spikeDone
	var ethSweepBlock uint64
	if err := pollUntil(ctx, 25*time.Minute, func() bool {
		_, n, found, _ := anvil.FindTxFrom(ctx, vault0, 1, w1Block)
		ethSweepBlock = n
		return found
	}); err != nil {
		t.Fatalf("the ETH sweep never mined after the spike\nbot:\n%s", botLogs())
	}
	timings["batch built -> ETH sweep mined (4 min spike, bot restart)"] = devnetSince(batchAt)

	// A shallow reorg drops the mined ETH sweep before it is final; the bot
	// must re-post it.
	if f, _ := anvil.Finalized(ctx); f < ethSweepBlock {
		head, _ := anvil.cli.BlockNumber(ctx)
		reorgAt := devnetNow()
		if err := anvil.rpc.CallContext(ctx, nil, "anvil_reorg", head-ethSweepBlock+1, []any{}); err != nil {
			t.Fatalf("anvil_reorg: %v", err)
		}
		if err := pollUntil(ctx, 20*time.Minute, func() bool {
			_, n, found, _ := anvil.FindTxFrom(ctx, vault0, 1, w1Block)
			ethSweepBlock = n
			return found
		}); err != nil {
			t.Fatalf("the reorged ETH sweep was never mined again (the bot must re-post it)\nbot:\n%s", botLogs())
		}
		timings["reorg of the ETH sweep -> mined again"] = devnetSince(reorgAt)
	} else {
		t.Logf("(W2) the ETH sweep was already final in block %d: no shallow reorg possible this run", ethSweepBlock)
	}
	var tokenSweepBlock uint64
	if err := pollUntil(ctx, 20*time.Minute, func() bool {
		_, n, found, _ := anvil.FindTxFrom(ctx, vault0, 2, w1Block)
		tokenSweepBlock = n
		return found
	}); err != nil {
		t.Fatalf("the USDC sweep never mined\nbot:\n%s", botLogs())
	}

	// The prover is late: the sweeps' headers land 5 minutes after they mine.
	devnetSleep(5 * time.Minute)
	for _, b := range []uint64{ethSweepBlock, tokenSweepBlock} {
		if err := e.plantFinalized(anvil, b); err != nil {
			t.Fatal(err)
		}
	}
	if err := pollUntil(ctx, 15*time.Minute, func() bool { return !strings.Contains(state("msr"), "0-1") && !strings.Contains(state("msr"), "0-2") }); err != nil {
		t.Fatalf("sweeps not confirmed: msr=%q\nbot:\n%s", state("msr"), botLogs())
	}

	// The window ends when the post-cut-over withdrawal leaves generation 1.
	var w2Block uint64
	if err := pollUntil(ctx, 25*time.Minute, func() bool {
		_, n, found, _ := anvil.FindTxFrom(ctx, vault1, 0, w1Block)
		w2Block = n
		return found
	}); err != nil {
		t.Fatalf("generation 1 never sent the queued withdrawal\nbot:\n%s", botLogs())
	}
	timings["USER WINDOW: activateKey -> first new-key withdrawal on L1"] = devnetSince(t0)
	if err := e.plantFinalized(anvil, w2Block); err != nil {
		t.Fatal(err)
	}
	if err := pollUntil(ctx, 10*time.Minute, func() bool { return state("n") == "1" && state("d-0") == "" }); err != nil {
		t.Fatalf("generation 1's first withdrawal not settled\nbot:\n%s", botLogs())
	}

	// Leftover sweep, retire, purge. Its header is planted once: an L1 call
	// every Hive block loses every L2 slot (SLOT-BOUNDARY-TX-1; rotation run
	// 4 halted on it).
	planted := false
	if err := pollUntil(ctx, 25*time.Minute, func() bool {
		if _, n, found, _ := anvil.FindTxFrom(ctx, vault0, 3, w1Block); found && !planted {
			planted = e.plantFinalized(anvil, n) == nil
		}
		return strings.Contains(botLogs(), "inactivated=0")
	}); err != nil {
		t.Fatalf("generation 0 not drained and retired: msr=%q eth book %q\nbot:\n%s", state("msr"), state("vb-0-eth"), botLogs())
	}
	_ = anvil.Mine(ctx, 70)
	head, _ := anvil.cli.BlockNumber(ctx)
	if err := e.plantFinalized(anvil, head); err != nil {
		t.Fatal(err)
	}
	if err := pollUntil(ctx, 10*time.Minute, func() bool { return strings.Contains(botLogs(), "purged=0") }); err != nil {
		t.Fatalf("generation 0 not purged\nbot:\n%s", botLogs())
	}

	// End state: every wei of custody at generation 1, the books empty, each
	// withdrawal paid once.
	bal1, _ := anvil.cli.BalanceAt(ctx, vault1, nil)
	left, _ := anvil.cli.BalanceAt(ctx, vault0, nil)
	if active, _ := new(big.Int).SetString(strings.Split(state("s-eth"), "|")[0], 10); active == nil || bal1.Cmp(active) < 0 {
		t.Fatalf("generation 1 holds %s wei, below the ETH custody %s", bal1, state("s-eth"))
	}
	if got, _ := anvil.ERC20BalanceOf(ctx, usdc, vault1); got.Cmp(usd(25)) != 0 {
		t.Fatalf("generation 1 holds %s USDC units, want 25000000", got)
	}
	if got, _ := anvil.cli.BalanceAt(ctx, recip2, nil); got.Cmp(big.NewInt(5e17)) != 0 {
		t.Fatalf("recipient 2 holds %s wei, want 0.5 ETH exactly once", got)
	}
	if left.Cmp(big.NewInt(1e15)) > 0 {
		t.Fatalf("%s wei left at generation 0 after its purge", left)
	}
	if np, n := state("np"), state("n"); np != n || state("wqh") != state("wqt") {
		t.Fatalf("withdrawals left: np=%s n=%s queue %s..%s", np, n, state("wqh"), state("wqt"))
	}
	for _, l := range strings.Split(botLogs(), "\n") {
		if strings.Contains(l, "panic") || strings.Contains(strings.ToLower(l), "dead-letter") || strings.Contains(l, "dead_letter") {
			t.Fatalf("bot log: %s", l)
		}
	}
	t.Logf("RPC faults met: A %s, B %s", rpcA, rpcB)
	for k, v := range timings {
		t.Logf("TIMING %-70s %s", k, v.Round(time.Second))
	}
	t.Logf("(W) PASS: rotation under mainnet weather; generation 1 holds %s wei and 25 USDC", bal1)
}
