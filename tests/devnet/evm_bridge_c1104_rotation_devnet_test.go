//go:build evm_devnet

// C1104 E1: the 11-04 testnet campaign's EVM fixes in a vault rotation under
// mainnet weather. The rotation and its weather are A01_Weather's (two flaky
// free-RPC stand-ins, a witness down through generation 1's keygen and another
// through its reshare, a fee spike with the bot restarted under it, a shallow
// reorg of the ETH sweep, late headers); on top of them each fix's own fault
// is injected where testnet met it. Every fix is a CHECK, recorded as FIXED or
// BUG without stopping the run, so one run of develop's builds (arm A) shows
// every bug and one run of the fix branches' builds (arm B) must show none:
//
//	I-15  unmapETH to a vault address: a retiring generation's, then the active one's
//	I-17  no no-op drainWithdrawalQueue while a superseded generation holds the nonces
//	PS-2  no re-drive of a sweep whose receipt cannot be read (it may have mined)
//	I-26  a deposit to the Inactive generation whose header is stored only after
//	      the purge grace passed is mapped before the purge, not lost to it
//
// The arms differ only in EVM_BRIDGE_DEVNET_WASM and EVM_BOT_BIN (the nodes are
// this tree's; their 11-04 changes are log lines and the election proposer):
//
//	eval "$(tests/devnet/suite/evm-artifacts.sh)"   # run from the arm's tree, ACCOUNT_MAPPING_DIR = the arm's account-mapping
//	go test -tags evm_devnet -run TestEVMBridge_C1104_RotationFaults ./tests/devnet/ -timeout 250m -v
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

func TestEVMBridge_C1104_RotationFaults(t *testing.T) {
	if testing.Short() {
		t.Skip("devnet")
	}
	ctx, e := a01Devnet(t, 240*time.Minute)
	anvil := StartAnvil(t, ctx, evmChainID, nextAnvilPort())
	t.Cleanup(anvil.Stop)
	eth := func(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(1e18)) }
	usd := func(n int64) *big.Int { return big.NewInt(n * 1_000_000) }
	state := func(k string) string { return e.stateStr(k) }
	timings := map[string]time.Duration{}

	// The checks, printed (and failed) at the end even if a later step stops the run.
	type check struct {
		name, detail string
		fixed        bool
	}
	var checks []check
	record := func(name string, fixed bool, detail string) {
		checks = append(checks, check{name, detail, fixed})
		verdict := "BUG"
		if fixed {
			verdict = "FIXED"
		}
		t.Logf("CHECK %s: %s (%s)", name, verdict, detail)
	}
	t.Cleanup(func() {
		for _, c := range checks {
			verdict := "BUG"
			if c.fixed {
				verdict = "FIXED"
			}
			t.Logf("C1104 SUMMARY %-6s %-5s %s", c.name, verdict, c.detail)
			if !c.fixed {
				t.Errorf("%s: %s", c.name, c.detail)
			}
		}
	})

	rpcA, err := StartChaosRPC(anvil.URL, MainnetWeather(1104))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rpcA.Stop)
	rpcB, err := StartChaosRPC(anvil.URL, MainnetWeather(1105))
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
		Checkpoint: filepath.Join(t.TempDir(), "c1104-checkpoint.json"),
	}
	bot := StartBot(t, ctx, botCfg)
	t.Cleanup(func() { bot.Stop() })
	var earlier []string
	allLogs := func() []string { return append(append([]string{}, earlier...), bot.Logs()...) }
	botLogs := func() string { return strings.Join(allLogs(), "\n") }
	// countSince counts bot log lines containing sub from line index from on.
	countSince := func(from int, sub string) int {
		n := 0
		for i, l := range allLogs() {
			if i >= from && strings.Contains(l, sub) {
				n++
			}
		}
		return n
	}
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
		tx, err := e.d.CallContractEvm(ctx, e.gqlNode, K, e.amID, "unmapETH", fmt.Sprintf(`{"to":"%s","amount":"%s","asset":"eth"}`, to.Hex(), wei), 5000000)
		if err != nil {
			t.Fatalf("unmapETH: %v", err)
		}
		// Wait for its output: the user's next call takes its nonce from the node,
		// and a call sent before this one lands reuses this nonce and is dropped
		// (arm A run 1, 23:4x: the I-15 call after recip2's never ran).
		if ok, msg := e.outResult(ctx, tx); !ok {
			t.Fatalf("unmapETH to %s refused: %s", to.Hex(), msg)
		}
	}
	// unmapTry is an unmapETH whose refusal is the expected outcome: it returns
	// whether the bridge accepted it.
	unmapTry := func(to ethcommon.Address, wei string) (bool, string) {
		tx, err := e.d.CallContractEvm(ctx, e.gqlNode, K, e.amID, "unmapETH", fmt.Sprintf(`{"to":"%s","amount":"%s","asset":"eth"}`, to.Hex(), wei), 5000000)
		if err != nil {
			return false, err.Error()
		}
		return e.outResult(ctx, tx)
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

	// The rotation with flaky witnesses: one down through generation 1's
	// keygen, another through its check signature and reshare.
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
	timings["createKey -> keygen (witness down 4 min)"] = devnetSince(ceremony)
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
	vault1, ok := tssKeyAddress(e.keyPub("primaryv1"))
	if !ok {
		t.Fatal("generation 1 key is not secp256k1")
	}
	t.Logf("(W1) cut over to %s after two witness outages", vault1.Hex())

	// A withdrawal after the cut-over waits for the hand-off.
	recip2 := ethCrypto.PubkeyToAddress(mustKey(t).PublicKey)
	unmap(recip2, "500000000000000000")
	// recip2's withdrawal now waits in the queue (the I-17 window starts once
	// the batch is built and generation 0's nonce hold is gone, below).
	// I-15, a retiring generation: the user names generation 0's address. Asked
	// after recip2's, so in arm A its payment (from generation 1, after the
	// hand-off) does not move recip2's nonce.
	if accepted, msg := unmapTry(vault0, "100000000000000000"); accepted {
		record("I-15", false, "unmapETH to the retiring generation-0 vault address was accepted: the bridge pays the user's ETH into its own vault")
	} else {
		record("I-15", true, "unmapETH to the retiring generation-0 vault refused: "+msg)
	}
	if err := e.plantFinalized(anvil, w1Block); err != nil {
		t.Fatal(err)
	}

	// The batch, then a fee spike above the sweeps' fee caps, and the bot
	// restarted under it.
	if err := pollUntil(ctx, 15*time.Minute, func() bool { return strings.Contains(state("msr"), "0-1") && strings.Contains(state("msr"), "0-2") }); err != nil {
		t.Fatalf("no batch: msr=%q\nbot:\n%s", state("msr"), botLogs())
	}
	// I-17 window: from here (the batch built and generation 0's nonce hold,
	// wnh, gone) until the sweeps are confirmed, generation 1 covers no queued
	// withdrawal (activeGenCovers) and every drain issues nothing and changes
	// nothing (testnet 06:57-08:17: one every ~70 s, 551 RC each). The drain
	// that clears wnh (gen 0's pending withdrawals confirmed) does real work
	// and is not counted (E1y 11-05: counted from the unmap, the check read it
	// as a no-op).
	_ = pollUntil(ctx, 5*time.Minute, func() bool { return state("wnh") == "" })
	drainFrom := len(allLogs())
	batchAt := devnetNow()
	spike := big.NewInt(60e9)
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
	timings["batch built -> ETH sweep mined (spike, bot restart)"] = devnetSince(batchAt)
	if f, _ := anvil.Finalized(ctx); f < ethSweepBlock {
		head, _ := anvil.cli.BlockNumber(ctx)
		if err := anvil.rpc.CallContext(ctx, nil, "anvil_reorg", head-ethSweepBlock+1, []any{}); err != nil {
			t.Fatalf("anvil_reorg: %v", err)
		}
		if err := pollUntil(ctx, 20*time.Minute, func() bool {
			_, n, found, _ := anvil.FindTxFrom(ctx, vault0, 1, w1Block)
			ethSweepBlock = n
			return found
		}); err != nil {
			t.Fatalf("the reorged ETH sweep was never mined again\nbot:\n%s", botLogs())
		}
	}
	var tokenSweepBlock uint64
	if err := pollUntil(ctx, 20*time.Minute, func() bool {
		_, n, found, _ := anvil.FindTxFrom(ctx, vault0, 2, w1Block)
		tokenSweepBlock = n
		return found
	}); err != nil {
		t.Fatalf("the USDC sweep never mined\nbot:\n%s", botLogs())
	}

	// PS-2: the sweeps are mined, and now neither RPC answers a receipt read
	// (both refuse eth_getTransactionReceipt). The bridge's stored L1 height
	// moves past the re-drive age; a bot that reads an unreadable receipt as
	// "not mined" re-drives a sweep that already mined (a version that can
	// never mine, a TSS signature, RC and booked gas for nothing).
	redriveFrom := len(allLogs())
	rpcA.SetMethodFault("eth_getTransactionReceipt", "429")
	rpcB.SetMethodFault("eth_getTransactionReceipt", "429")
	_ = anvil.Mine(ctx, 40)
	if head, err := anvil.cli.BlockNumber(ctx); err == nil {
		_ = e.plantFinalized(anvil, head)
	}
	devnetSleep(5 * time.Minute)
	redrives := countSince(redriveFrom, "sweep re-drive submitted")
	rpcA.SetMethodFault("eth_getTransactionReceipt", "")
	rpcB.SetMethodFault("eth_getTransactionReceipt", "")
	record("PS-2", redrives == 0, fmt.Sprintf("%d re-drive(s) of a mined sweep in 5 min of unreadable receipts", redrives))

	// The prover is late: the sweeps' headers land after they mine.
	for _, b := range []uint64{ethSweepBlock, tokenSweepBlock} {
		if err := e.plantFinalized(anvil, b); err != nil {
			t.Fatal(err)
		}
	}
	if err := pollUntil(ctx, 15*time.Minute, func() bool { return !strings.Contains(state("msr"), "0-1") && !strings.Contains(state("msr"), "0-2") }); err != nil {
		t.Fatalf("sweeps not confirmed: msr=%q\nbot:\n%s", state("msr"), botLogs())
	}
	drains := countSince(drainFrom, "action=drainWithdrawalQueue")
	record("I-17", drains == 0, fmt.Sprintf("%d drainWithdrawalQueue broadcast(s) while the sweeps were open, no nonce hold remained and a withdrawal was queued (each would issue nothing)", drains))

	// The hand-off: the queued withdrawal leaves generation 1.
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

	// Leftover sweep and retire (header planted once: SLOT-BOUNDARY-TX-1).
	planted := false
	if err := pollUntil(ctx, 25*time.Minute, func() bool {
		if _, n, found, _ := anvil.FindTxFrom(ctx, vault0, 3, w1Block); found && !planted {
			planted = e.plantFinalized(anvil, n) == nil
		}
		return strings.Contains(botLogs(), "inactivated=0")
	}); err != nil {
		t.Fatalf("generation 0 not drained and retired: msr=%q eth book %q\nbot:\n%s", state("msr"), state("vb-0-eth"), botLogs())
	}

	// I-26: generation 0 is Inactive (still credited). A second user pays it;
	// the prover is slow, so that block's header is stored only together with
	// a head past the purge grace (testnet 09:10: purge 09:10:12, the map 39 s
	// later refused, 0.02 ETH stranded at an address the bridge never signs
	// from again).
	dep2Key := mustKey(t)
	dep2Addr := ethCrypto.PubkeyToAddress(dep2Key.PublicKey)
	user2 := evmDID(dep2Addr)
	if err := anvil.SetBalance(ctx, dep2Addr, eth(1)); err != nil {
		t.Fatal(err)
	}
	_, dep2Block, err := anvil.SendETH(ctx, dep2Key, vault0, big.NewInt(5e16))
	if err != nil {
		t.Fatal(err)
	}
	_ = anvil.Mine(ctx, 70)
	head, _ := anvil.cli.BlockNumber(ctx)
	if err := e.plantFinalized(anvil, head); err != nil {
		t.Fatal(err)
	}
	lateAt := devnetNow()
	// The deposit's own header lands late, as on testnet 11-04 09:10 (stored by
	// a later proof's attestation): the head is past the purge grace first and
	// the deposit waits parked for its header. The mock verifier stores headers
	// only by devSetHeader (no attestHistoryRoot / attestHeaders / submitHeaders),
	// so the test plants it, 5 min after the head (E1x 11-05: develop's bot
	// purged 3m34s after the head). (11-05 01:4x-02:5x this planted the bot's
	// prefetched roots instead, which the mock cannot attest from.)
	dep2Planted := false
	credited, purgedFirst := false, false
	_ = pollUntil(ctx, 12*time.Minute, func() bool {
		if !dep2Planted && devnetSince(lateAt) > 5*time.Minute {
			dep2Planted = true
			if err := e.plantFinalized(anvil, dep2Block); err != nil {
				t.Logf("I-26: planting the deposit block %d: %v", dep2Block, err)
			}
		}
		credited = state("a-"+user2+"-eth") != "" && state("a-"+user2+"-eth") != "0"
		if !credited && evmGenPurged(ctx, e, 0) {
			purgedFirst = true
		}
		return credited || purgedFirst
	})
	if purgedFirst && !credited {
		// Give a map one more chance after the purge: a refused map is the bug.
		devnetSleep(2 * time.Minute)
		credited = state("a-"+user2+"-eth") != "" && state("a-"+user2+"-eth") != "0"
	}
	record("I-26", credited && !purgedFirst, fmt.Sprintf("deposit to the Inactive generation: credited=%v purged before its map=%v (%s after its header)", credited, purgedFirst, devnetSince(lateAt).Round(time.Second)))

	// The purge follows (on the fix, a map window after the grace).
	_ = anvil.Mine(ctx, 70)
	head, _ = anvil.cli.BlockNumber(ctx)
	if err := e.plantFinalized(anvil, head); err != nil {
		t.Fatal(err)
	}
	// The credited late deposit put generation 0 back to Draining (by design),
	// so the bot sweeps it again and asks for that sweep's block header. On the
	// mock verifier only the test stores headers (no attestation or link-down):
	// plant every block the bot logs it needs, as the operator's proof and an
	// attestation would store it (E1y 11-05: header 4296 never came, so
	// confirmSweep 0-4 could not run and the purge never followed).
	// The purge itself waits on the STORED L1 height (grace + map window past
	// the last emptying), which on the mock moves only when the test plants a
	// header: keep planting a fresh head, as the operator's regular proofs would
	// (E1z 11-05: re-swept, confirmed and inactivated at 4436, then nothing
	// stored above it, so the purge could never come).
	plantedNeed := map[uint64]bool{}
	lastNeedScan, lastHead := time.Time{}, time.Time{}
	if err := pollUntil(ctx, 10*time.Minute, func() bool {
		if devnetSince(lastHead) > time.Minute {
			lastHead = devnetNow()
			if h, err := anvil.cli.BlockNumber(ctx); err == nil {
				_ = anvil.Mine(ctx, 70)
				_ = e.plantFinalized(anvil, h)
			}
		}
		if devnetSince(lastNeedScan) > 20*time.Second {
			lastNeedScan = devnetNow()
			for _, l := range allLogs() {
				i := strings.Index(l, "header needed block=")
				if i < 0 {
					continue
				}
				var b uint64
				if _, err := fmt.Sscanf(l[i+len("header needed block="):], "%d", &b); err == nil && b > 0 && !plantedNeed[b] {
					plantedNeed[b] = true
					if err := e.plantFinalized(anvil, b); err != nil {
						t.Logf("planting needed header %d: %v", b, err)
					}
				}
			}
		}
		return evmGenPurged(ctx, e, 0)
	}); err != nil {
		t.Fatalf("generation 0 not purged (planted needed headers %v)\nbot:\n%s", plantedNeed, botLogs())
	}

	// I-15, the active generation: the user names generation 1's own address.
	if accepted, msg := unmapTry(vault1, "100000000000000000"); accepted {
		record("I-15a", false, "unmapETH to the active generation-1 vault address was accepted: the bridge pays the user's ETH to itself")
	} else {
		record("I-15a", true, "unmapETH to the active vault refused: "+msg)
	}

	// Every withdrawal accepted above settles (arm A pays the vault-address ones).
	_ = anvil.Mine(ctx, 3)
	head, _ = anvil.cli.BlockNumber(ctx)
	_ = e.plantFinalized(anvil, head)
	_ = pollUntil(ctx, 10*time.Minute, func() bool { return state("np") == state("n") && state("wqh") == state("wqt") })

	// End state: custody at generation 1, recip2 paid once, nothing lost to dead letters.
	bal1, _ := anvil.cli.BalanceAt(ctx, vault1, nil)
	if active, _ := new(big.Int).SetString(strings.Split(state("s-eth"), "|")[0], 10); active == nil || bal1.Cmp(active) < 0 {
		t.Errorf("generation 1 holds %s wei, below the ETH custody %s", bal1, state("s-eth"))
	}
	if got, _ := anvil.ERC20BalanceOf(ctx, usdc, vault1); got.Cmp(usd(25)) != 0 {
		t.Errorf("generation 1 holds %s USDC units, want 25000000", got)
	}
	if got, _ := anvil.cli.BalanceAt(ctx, recip2, nil); got.Cmp(big.NewInt(5e17)) != 0 {
		t.Errorf("recipient 2 holds %s wei, want 0.5 ETH exactly once", got)
	}
	if np, n := state("np"), state("n"); np != n || state("wqh") != state("wqt") {
		t.Errorf("withdrawals left: np=%s n=%s queue %s..%s", np, n, state("wqh"), state("wqt"))
	}
	deadLetters := 0
	for _, l := range allLogs() {
		if strings.Contains(l, "panic") {
			t.Errorf("bot log: %s", l)
		}
		if strings.Contains(strings.ToLower(l), "dead-letter") || strings.Contains(l, "dead_letter") {
			deadLetters++
		}
	}
	t.Logf("bot dead-letter lines: %d", deadLetters)
	t.Logf("RPC faults met: A %s, B %s", rpcA, rpcB)
	for k, v := range timings {
		t.Logf("TIMING %-70s %s", k, v.Round(time.Second))
	}
}
