//go:build evm_devnet

// C1104 E2: the 11-04 testnet campaign's EVM bot fixes whose fault a devnet can
// force on purpose, under mainnet weather (two flaky free-RPC stand-ins on the
// bot's L1 reads, as A01_Weather). Each fix is a CHECK, recorded as FIXED or BUG
// without stopping the run, so one run of develop's bot (arm A) shows the bugs
// and one run of the fix branch's bot (arm B) must show none:
//
//	I-19  after more than 5 minutes with no L2 call, two calls in one tick each
//	      get their own nonce (B-R1b reused the first one's: the second was lost
//	      while logged as sent)
//	I-13  a withdrawal paid on L1 is confirmed while the bridge is paused (the
//	      bot used to skip its whole tick and back off to 10 minutes)
//	I-8   a call accepted by a node that is cut off from its peers (it never
//	      gossips it) reaches the chain through the bot's other endpoint
//	PS-5  with more than 100 deposit blocks waiting for headers, the ones whose
//	      headers are stored still map (a read over the node's 100-key limit
//	      failed every tick, so nothing mapped)
//
// The RPC- and RC-path fixes (I-5, I-18, I-20) and the operator's (I-23) need
// the real verifier and paid proofs; they are run on testnet, where they were
// found, against the fixed bot and operator.
//
//	eval "$(tests/devnet/suite/evm-artifacts.sh)"   # run from the arm's tree
//	go test -tags evm_devnet -run TestEVMBridge_C1104_BotFaults ./tests/devnet/ -timeout 200m -v
package devnet

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"fmt"
	"math/big"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"vsc-node/lib/dids"

	ethcommon "github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

// sendETHNoWait signs and sends a value transfer without waiting for a receipt
// (so several can share a block while automine is off).
func (a *Anvil) sendETHNoWait(ctx context.Context, key *ecdsa.PrivateKey, nonce uint64, to ethcommon.Address, wei *big.Int) (ethcommon.Hash, error) {
	tx := ethtypes.NewTx(&ethtypes.DynamicFeeTx{
		ChainID: a.ChainID, Nonce: nonce, GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(2e10),
		Gas: 21000, To: &to, Value: wei,
	})
	signed, err := ethtypes.SignTx(tx, ethtypes.LatestSignerForChainID(a.ChainID), key)
	if err != nil {
		return ethcommon.Hash{}, err
	}
	return signed.Hash(), a.cli.SendTransaction(ctx, signed)
}

func TestEVMBridge_C1104_BotFaults(t *testing.T) {
	if testing.Short() {
		t.Skip("devnet")
	}
	ctx, e := a01Devnet(t, 190*time.Minute)
	anvil := StartAnvil(t, ctx, evmChainID, nextAnvilPort())
	t.Cleanup(anvil.Stop)
	eth := func(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(1e18)) }
	milli := func(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(1e15)) }

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

	rpcA, err := StartChaosRPC(anvil.URL, MainnetWeather(2104))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rpcA.Stop)
	rpcB, err := StartChaosRPC(anvil.URL, MainnetWeather(2105))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rpcB.Stop)

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
	// I-8 needs two GraphQL endpoints: the bot posts to the first (X, cut off
	// later) and reads state through it; the second is the test's own node.
	X := 0
	for n := 2; n <= 5; n++ {
		if n != e.gqlNode && n != e.ownerNode {
			X = n
			break
		}
	}
	reader := e.gqlNode
	stateAt := func(k string) string {
		m, err := e.d.GetStateByKeys(ctx, reader, e.amID, []string{k})
		if err != nil || m == nil || m[k] == nil {
			return ""
		}
		return fmt.Sprintf("%v", m[k])
	}
	botCfg := BotConfig{
		EthRPC: rpcA.URL + "," + rpcB.URL, ContractID: e.amID,
		GraphQLURL: e.d.GQLEndpoint(X) + "," + e.d.GQLEndpoint(e.gqlNode),
		EthPrivHex: hex.EncodeToString(ethCrypto.FromECDSA(botKey)),
		Checkpoint: filepath.Join(t.TempDir(), "c1104e2-checkpoint.json"),
	}
	bot := StartBot(t, ctx, botCfg)
	t.Cleanup(func() { bot.Stop() })
	botLogs := func() string { return strings.Join(bot.Logs(), "\n") }
	if !bot.WaitForLog(ctx, "A18: vault is the TSS key's address", evmL2Wait+15*time.Minute) {
		t.Fatalf("the bot did not take the vault:\n%s", botLogs())
	}
	credited := func(user string) *big.Int {
		v, _ := new(big.Int).SetString(stateAt("a-"+user+"-eth"), 10)
		if v == nil {
			return big.NewInt(0)
		}
		return v
	}

	// plantFresh stores a just-mined block's header. anvil's finalized block
	// trails the latest by ~64 (scratch anvil 11-05: latest 11, finalized 0) and
	// plantFinalized waits 60 s for it after mining 6, so a fresh block passed
	// only by luck at one block a second; mine past it first, as the rotation
	// test does.
	plantFresh := func(b uint64) error {
		_ = anvil.Mine(ctx, 70)
		return e.plantFinalized(anvil, b)
	}

	// (0) A baseline deposit maps through the flaky RPCs.
	u0Key := mustKey(t)
	u0 := evmDID(ethCrypto.PubkeyToAddress(u0Key.PublicKey))
	if err := anvil.SetBalance(ctx, ethCrypto.PubkeyToAddress(u0Key.PublicKey), eth(5)); err != nil {
		t.Fatal(err)
	}
	_, b0, err := anvil.SendETH(ctx, u0Key, vault0, eth(2))
	if err != nil {
		t.Fatal(err)
	}
	if err := plantFresh(b0); err != nil {
		t.Fatal(err)
	}
	if err := pollUntil(ctx, 12*time.Minute, func() bool { return credited(u0).Sign() > 0 }); err != nil {
		t.Fatalf("baseline deposit not mapped\nbot:\n%s", botLogs())
	}
	t.Logf("(0) baseline deposit mapped: %s wei", credited(u0))

	// (1) I-19. Six minutes with no L2 call from the bot, then two deposits in
	// one block: the bot maps both in one tick, two quick calls.
	devnetSleep(6 * time.Minute)
	u1Key, u2Key := mustKey(t), mustKey(t)
	u1A, u2A := ethCrypto.PubkeyToAddress(u1Key.PublicKey), ethCrypto.PubkeyToAddress(u2Key.PublicKey)
	u1, u2 := evmDID(u1A), evmDID(u2A)
	for _, a := range []ethcommon.Address{u1A, u2A} {
		if err := anvil.SetBalance(ctx, a, eth(1)); err != nil {
			t.Fatal(err)
		}
	}
	// Both deposits in one block: stop the one-a-second mining, send, mine one
	// block, resume (the W9 weather test's pattern).
	if err := anvil.rpc.CallContext(ctx, nil, "evm_setIntervalMining", 0); err != nil {
		t.Fatalf("evm_setIntervalMining 0: %v", err)
	}
	h1, err1 := anvil.sendETHNoWait(ctx, u1Key, 0, vault0, milli(300))
	h2, err2 := anvil.sendETHNoWait(ctx, u2Key, 0, vault0, milli(200))
	_ = anvil.rpc.CallContext(ctx, nil, "evm_mine")
	// Back to one block a second: evm_setAutomine(true) would switch anvil to
	// mining only on a transaction, and the finalized block would stop moving
	// (E2 arm A 11-05: 'block 855 not finalized'; scratch anvil: 7 -> 7 in 4 s
	// after it, 7 -> 11 after evm_setIntervalMining(1)).
	if err := anvil.rpc.CallContext(ctx, nil, "evm_setIntervalMining", 1); err != nil {
		t.Fatalf("evm_setIntervalMining: %v", err)
	}
	if err1 != nil || err2 != nil {
		t.Fatalf("send: %v %v", err1, err2)
	}
	r1, _ := anvil.waitReceipt(ctx, h1, 30*time.Second)
	r2, _ := anvil.waitReceipt(ctx, h2, 30*time.Second)
	if r1 == nil || r2 == nil || r1.BlockNumber.Uint64() != r2.BlockNumber.Uint64() {
		t.Fatalf("the two deposits are not in one block: %v %v", r1, r2)
	}
	mapFrom := len(bot.Logs())
	if err := plantFresh(r1.BlockNumber.Uint64()); err != nil {
		t.Fatal(err)
	}
	both := pollUntil(ctx, 10*time.Minute, func() bool { return credited(u1).Sign() > 0 && credited(u2).Sign() > 0 }) == nil
	// The evidence of B-R1b: two map broadcasts at one nonce.
	nonces := map[string]int{}
	re := regexp.MustCompile(`action=map nonce=(\d+)`)
	for i, l := range bot.Logs() {
		if i >= mapFrom {
			if m := re.FindStringSubmatch(l); m != nil {
				nonces[m[1]]++
			}
		}
	}
	dup := 0
	for _, c := range nonces {
		if c > 1 {
			dup++
		}
	}
	record("I-19", both && dup == 0, fmt.Sprintf("two maps in one tick after 6 min quiet: both credited within 10 min=%v, nonces used twice=%d", both, dup))

	// (2) I-13. A withdrawal is paid on L1, then the bridge is paused before its
	// header lands: confirmSpend is not pause-gated and must still go.
	K0, err := NewEthKey(hex.EncodeToString(ethCrypto.FromECDSA(u0Key)))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.fundDID(u0, "5000.000"); err != nil {
		t.Fatal(err)
	}
	recip := ethCrypto.PubkeyToAddress(mustKey(t).PublicKey)
	if _, err := e.d.CallContractEvm(ctx, reader, K0, e.amID, "unmapETH", fmt.Sprintf(`{"to":"%s","amount":"500000000000000000","asset":"eth"}`, recip.Hex()), 5000000); err != nil {
		t.Fatalf("unmapETH: %v", err)
	}
	var wBlock uint64
	if err := pollUntil(ctx, 20*time.Minute, func() bool {
		_, n, found, _ := anvil.FindTxFrom(ctx, vault0, 0, b0)
		wBlock = n
		return found
	}); err != nil {
		t.Fatalf("the withdrawal never reached L1\nbot:\n%s", botLogs())
	}
	if err := e.proposeExecute("pause", "{}"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := pollUntil(ctx, evmL2Wait, func() bool { return stateAt("paused") == "1" }); err != nil {
		t.Fatal("pause did not set paused=1")
	}
	pausedAt := devnetNow()
	if err := plantFresh(wBlock); err != nil {
		t.Fatal(err)
	}
	confirmed := pollUntil(ctx, 8*time.Minute, func() bool { return stateAt("n") == "1" }) == nil
	record("I-13", confirmed, fmt.Sprintf("paid withdrawal confirmed while paused=%v (%s after its header)", confirmed, devnetSince(pausedAt).Round(time.Second)))
	if err := e.proposeExecute("unpause", "{}"); err != nil {
		t.Fatalf("unpause: %v", err)
	}
	if err := pollUntil(ctx, 12*time.Minute, func() bool { return stateAt("n") == "1" }); err != nil {
		t.Fatalf("the withdrawal never settled after the unpause\nbot:\n%s", botLogs())
	}

	// (3) I-8. The bot's first endpoint X is cut off from every other node: it
	// accepts the bot's call and never gossips it (testnet 03:54: nonces 29-35
	// held 30+ min). A deposit's map must reach the chain through the bot's
	// other endpoint while X is still cut off.
	nodes := []int{}
	for n := 1; n <= e.d.cfg.Nodes; n++ {
		if n != X {
			nodes = append(nodes, n)
		}
	}
	for _, n := range nodes {
		if err := e.d.Partition(ctx, X, n); err != nil {
			t.Fatalf("partition magi-%d/magi-%d: %v", X, n, err)
		}
	}
	u3Key := mustKey(t)
	u3A := ethCrypto.PubkeyToAddress(u3Key.PublicKey)
	u3 := evmDID(u3A)
	if err := anvil.SetBalance(ctx, u3A, eth(1)); err != nil {
		t.Fatal(err)
	}
	_, b3, err := anvil.SendETH(ctx, u3Key, vault0, milli(100))
	if err != nil {
		t.Fatal(err)
	}
	if err := plantFresh(b3); err != nil {
		t.Fatal(err)
	}
	cutAt := devnetNow()
	landed := pollUntil(ctx, 10*time.Minute, func() bool { return credited(u3).Sign() > 0 }) == nil
	record("I-8", landed, fmt.Sprintf("map accepted by a cut-off node reached the chain through the other endpoint=%v (%s)", landed, devnetSince(cutAt).Round(time.Second)))
	for _, n := range nodes {
		_ = e.d.Heal(ctx, X, n)
	}
	// After the heal the cut-off node still holds the map unforwarded (no
	// re-gossip: followups 18ah, N1); only a re-post lands it. Recorded, not
	// fatal, so the PS-5 step below still runs (E2r arm A 11-05: develop's bot
	// never mapped it in 15 min after the heal and the run stopped here).
	healed := pollUntil(ctx, 15*time.Minute, func() bool { return credited(u3).Sign() > 0 }) == nil
	record("I-8b", healed, fmt.Sprintf("the stuck map landed within 15 min after the partition healed=%v", healed))

	// (4) PS-5. A prover outage: 101 deposit blocks wait for their headers (an
	// outage backlog), then the first five headers land. The bot must map
	// those five; a read of 101 header keys at once is refused by the node.
	u4Key := mustKey(t)
	u4A := ethCrypto.PubkeyToAddress(u4Key.PublicKey)
	u4 := evmDID(u4A)
	if err := anvil.SetBalance(ctx, u4A, eth(3)); err != nil {
		t.Fatal(err)
	}
	var blocks []uint64
	for i := 0; i < 101; i++ {
		_, b, err := anvil.SendETH(ctx, u4Key, vault0, milli(10))
		if err != nil {
			t.Fatalf("deposit %d: %v", i, err)
		}
		blocks = append(blocks, b)
	}
	_ = anvil.Mine(ctx, 10)
	if err := pollUntil(ctx, 10*time.Minute, func() bool { return strings.Count(botLogs(), "deposits detected") >= 3+101 }); err != nil {
		t.Logf("(4) the bot detected fewer than 101 new deposit blocks within 10 min (continuing)")
	}
	for _, b := range blocks[:5] {
		if err := plantFresh(b); err != nil {
			t.Fatal(err)
		}
	}
	want := new(big.Int).Mul(milli(10), big.NewInt(5))
	mapped := pollUntil(ctx, 12*time.Minute, func() bool {
		// 5 x 0.01 ETH less the deposit fee: at least 4.5 x 0.01 credited.
		return credited(u4).Cmp(new(big.Int).Div(new(big.Int).Mul(want, big.NewInt(9)), big.NewInt(10))) >= 0
	}) == nil
	record("PS-5", mapped, fmt.Sprintf("with 101 deposit blocks waiting, the 5 whose headers were stored mapped=%v (credited %s wei)", mapped, credited(u4)))

	for _, l := range bot.Logs() {
		if strings.Contains(l, "panic") {
			t.Errorf("bot log: %s", l)
		}
	}
	t.Logf("RPC faults met: A %s, B %s", rpcA, rpcB)
}
