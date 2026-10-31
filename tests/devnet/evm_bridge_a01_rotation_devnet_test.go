//go:build evm_devnet

// A01: the EVM bridge's vault rolls its key on a live devnet, driven by the
// real bot as the vault operator, with real TSS on every step: generation 1's
// keygen, its check signature and its pending reshare (TssGetKey fields 4 and
// 5), the owner's activateKey while a generation-0 withdrawal is still in
// flight (it must not wait), the one-batch sweep (ETH first, then USDC) signed
// by generation 0's key while a witness is down, the nonce hand-off, the first
// withdrawal from generation 1's key, the leftover sweep, retire and purge.
// The mock verifier only plants the L1 headers a prover would deliver.
//
// It logs the user-visible window: from activateKey until the first
// withdrawal requested after it is sent on L1 by the new key.
//
//	eval "$(tests/devnet/suite/evm-artifacts.sh)"
//	go test -tags evm_devnet -run TestEVMBridge_A01_VaultRotation ./tests/devnet/ -timeout 180m -v
package devnet

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"vsc-node/lib/dids"
	"vsc-node/modules/common/params"

	ethcommon "github.com/ethereum/go-ethereum/common"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

// a01Devnet is a devnet of the wave's own, as the custody wave's: the chain on
// 0.13 from epoch 1 (the A01 node rules' line), a short TSS rotate interval,
// and this wave's bridge in every node's EVM bridge map, which those rules key
// on (the new key's check signature, the reshare skip, the signer set, the
// bond lock, output scoping); the nodes restart on it.
func a01Devnet(t *testing.T, timeout time.Duration) (context.Context, *evmBridgeEnv) {
	t.Helper()
	cfg := evmBridgeConfig()
	cp := cfg.SysConfigOverrides.ConsensusParams
	cp.ConsensusVersionFloorMajor, cp.ConsensusVersionFloorConsensus, cp.ConsensusVersionFloorEpoch = 0, 13, 1
	if cfg.SysConfigOverrides.TssParams == nil {
		cfg.SysConfigOverrides.TssParams = &params.TssParams{}
	}
	cfg.SysConfigOverrides.TssParams.RotateInterval = 60
	d, ctx := startDevnetNoKey(t, cfg, timeout)
	if err := bootstrapEvmOwner(ctx, d); err != nil {
		t.Fatal(err)
	}
	e := setupEvmBridgeOn(t, ctx, d)
	if d.cfg.SysConfigOverrides.OracleParams == nil {
		d.cfg.SysConfigOverrides.OracleParams = &params.OracleParams{}
	}
	d.cfg.SysConfigOverrides.OracleParams.EvmBridgeContracts = map[string]string{"ETH": e.amID}
	d.cfg.SysConfigOverrides.OracleParams.EvmBridgeFromHeight = map[string]uint64{"ETH": 0}
	if err := writeSysConfigOverrides(d.cfg, d.devnetDir); err != nil {
		t.Fatal(err)
	}
	if err := d.RestartAllMagiNodes(ctx); err != nil {
		t.Fatal(err)
	}
	if err := pollUntil(ctx, 5*time.Minute, func() bool { _, _, err := d.LocalNodeInfo(ctx, e.gqlNode); return err == nil }); err != nil {
		t.Fatal("nodes did not come back after the bridge-map restart")
	}
	if err := waitForConsensus(ctx, d, "0.13", 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	return ctx, e
}

// keyPub is the public key of the bridge's TSS key name ("primaryv1"), "" until its keygen lands.
func (e *evmBridgeEnv) keyPub(name string) string {
	const q = `query($id:String!){ getTssKey(keyId:$id){ public_key } }`
	var out struct {
		GetTssKey *struct {
			PublicKey string `json:"public_key"`
		} `json:"getTssKey"`
	}
	if err := e.d.gqlQuery(e.ctx, e.gqlNode, q, map[string]any{"id": e.amID + "-" + name}, &out); err != nil || out.GetTssKey == nil {
		return ""
	}
	return out.GetTssKey.PublicKey
}

// ownerCall runs a direct owner export and returns whether its output succeeded.
func (e *evmBridgeEnv) ownerCall(action, payload string) (bool, string) {
	tx, err := e.callOnce(e.ownerNode, e.amID, action, payload)
	if err != nil {
		return false, err.Error()
	}
	return e.outResult(e.ctx, tx)
}

func TestEVMBridge_A01_VaultRotation(t *testing.T) {
	if testing.Short() {
		t.Skip("devnet")
	}
	ctx, e := a01Devnet(t, 170*time.Minute)
	anvil := StartAnvil(t, ctx, evmChainID, nextAnvilPort())
	t.Cleanup(anvil.Stop)
	eth := func(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(1e18)) }
	usd := func(n int64) *big.Int { return big.NewInt(n * 1_000_000) }
	state := func(k string) string { return e.stateStr(k) }

	// USDC on L1, registered on the bridge.
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

	// The bot: relayer, RC-funded, and (below) the vault operator.
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
	// Unbooked ETH at the vault (gas for the withdrawals, as the other waves):
	// stray ETH the proven sweep must carry too.
	if err := anvil.SetBalance(ctx, vault0, eth(1)); err != nil {
		t.Fatal(err)
	}
	bot := StartBot(t, ctx, BotConfig{
		EthRPC: anvil.URL, ContractID: e.amID, GraphQLURL: e.d.GQLEndpoint(e.gqlNode),
		EthPrivHex: hex.EncodeToString(ethCrypto.FromECDSA(botKey)), Tokens: usdc.Hex(),
	})
	t.Cleanup(bot.Stop)
	botLogs := func() string { return strings.Join(bot.Logs(), "\n") }
	if !bot.WaitForLog(ctx, "A18: vault is the TSS key's address", evmL2Wait+10*time.Minute) {
		t.Fatalf("the bot did not take the vault:\n%s", botLogs())
	}
	if ok, msg := e.ownerCall("setVaultOperator", botDID); !ok {
		t.Fatalf("setVaultOperator: %s", msg)
	}
	t.Logf("(0) vault 0 %s live; bot %s is relayer and vault operator", vault0.Hex(), botDID)

	// (1) Deposits to generation 0: 6 ETH and 25 USDC.
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
	if err := e.plantFinalized(anvil, ethBlock); err != nil {
		t.Fatal(err)
	}
	if err := e.plantFinalized(anvil, rc.BlockNumber.Uint64()); err != nil {
		t.Fatal(err)
	}
	if err := pollUntil(ctx, 8*time.Minute, func() bool {
		return state("a-"+user+"-eth") == "5940000000000000000" && state("a-"+user+"-usdc") == "25000000"
	}); err != nil {
		t.Fatalf("deposits not mapped: eth %q usdc %q\nbot:\n%s", state("a-"+user+"-eth"), state("a-"+user+"-usdc"), botLogs())
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
	t.Logf("(1) PASS: 6 ETH and 25 USDC mapped at generation 0")

	// (2) A withdrawal from generation 0, sent on L1 but not settled (its
	// header is not planted yet): in flight at the cut-over.
	recip1 := ethCrypto.PubkeyToAddress(mustKey(t).PublicKey)
	unmap(recip1, "1000000000000000000")
	var w1Block uint64
	if err := pollUntil(ctx, 20*time.Minute, func() bool {
		_, n, found, ferr := anvil.FindTxFrom(ctx, vault0, 0, ethBlock)
		w1Block = n
		return ferr == nil && found
	}); err != nil {
		t.Fatalf("generation 0's withdrawal never reached L1\nbot:\n%s", botLogs())
	}
	if state("np") != "1" || (state("n") != "" && state("n") != "0") { // n 0 is stored as no key
		t.Fatalf("n=%q np=%q, want the withdrawal in flight", state("n"), state("np"))
	}
	t.Logf("(2) generation 0's withdrawal mined in block %d, not yet settled", w1Block)

	// (3) The rotation. createKey; the network does generation 1's keygen,
	// check signature and pending reshare on its own; activateKey is retried
	// until the key is checked and reshared (the old generation serves
	// meanwhile), then must cut over with the withdrawal still in flight.
	if ok, msg := e.ownerCall("createKey", "{}"); !ok {
		t.Fatalf("createKey: %s", msg)
	}
	if err := pollUntil(ctx, 25*time.Minute, func() bool { return e.keyPub("primaryv1") != "" }); err != nil {
		t.Fatal("generation 1's keygen never landed")
	}
	t.Logf("(3a) generation 1 keygen: %s", e.keyPub("primaryv1"))
	var t0 time.Time
	var last string
	if err := pollUntil(ctx, 40*time.Minute, func() bool {
		ok, msg := e.ownerCall("activateKey", "{}")
		last = msg
		if ok {
			t0 = devnetNow()
		} else {
			devnetSleep(45 * time.Second)
		}
		return ok
	}); err != nil {
		t.Fatalf("activateKey never succeeded; last: %s", last)
	}
	vault1, ok := tssKeyAddress(e.keyPub("primaryv1"))
	if !ok {
		t.Fatal("generation 1 key is not secp256k1")
	}
	if state("wnh") != "0" || state("wnc-0") != "1" {
		t.Fatalf("cut-over state wnh=%q wnc-0=%q, want generation 0 holding nonce 0 with ceiling 1", state("wnh"), state("wnc-0"))
	}
	t.Logf("(3b) PASS: activateKey cut over to generation 1 (%s) with a withdrawal in flight", vault1.Hex())

	// (4) A withdrawal requested after the cut-over waits (hand-off, sweep).
	recip2 := ethCrypto.PubkeyToAddress(mustKey(t).PublicKey)
	unmap(recip2, "500000000000000000")
	if err := pollUntil(ctx, evmL2Wait, func() bool { return state("wqt") != "" && state("wqt") != state("wqh") }); err != nil {
		t.Fatalf("the post-cut-over withdrawal is not queued: wqh=%q wqt=%q", state("wqh"), state("wqt"))
	}

	// (5) Generation 0's withdrawal settles once its header lands.
	if err := e.plantFinalized(anvil, w1Block); err != nil {
		t.Fatal(err)
	}
	if err := pollUntil(ctx, 8*time.Minute, func() bool { return state("n") == "1" || state("wnh") == "" }); err != nil {
		t.Fatalf("generation 0's withdrawal never settled: n=%q\nbot:\n%s", state("n"), botLogs())
	}
	t.Logf("(5) PASS: generation 0's in-flight withdrawal settled after the cut-over")

	// (6) The bot builds the batch from a balance proof at that header. A
	// witness drops while generation 0's key signs it (flaky mainnet weather).
	if err := pollUntil(ctx, 10*time.Minute, func() bool { return strings.Contains(state("msr"), "0-1") && strings.Contains(state("msr"), "0-2") }); err != nil {
		t.Fatalf("no batch: msr=%q\nbot:\n%s", state("msr"), botLogs())
	}
	down := 2
	for down == e.gqlNode || down == e.ownerNode {
		down++
	}
	if err := e.d.StopNode(ctx, down); err != nil {
		t.Fatalf("stop magi-%d: %v", down, err)
	}
	t.Logf("(6) batch built (msr=%s); magi-%d stopped while it signs", state("msr"), down)
	var s1, s2 uint64
	if err := pollUntil(ctx, 25*time.Minute, func() bool {
		_, n1, f1, _ := anvil.FindTxFrom(ctx, vault0, 1, w1Block)
		_, n2, f2, _ := anvil.FindTxFrom(ctx, vault0, 2, w1Block)
		s1, s2 = n1, n2
		return f1 && f2
	}); err != nil {
		t.Fatalf("the sweeps never reached L1\nbot:\n%s", botLogs())
	}
	if err := e.d.StartNode(ctx, down); err != nil {
		t.Fatalf("start magi-%d: %v", down, err)
	}
	t.Logf("(6) PASS: both sweeps signed by generation 0 with magi-%d down, mined in blocks %d and %d", down, s1, s2)

	// (7) Their headers land; the bot confirms both.
	top := s1
	if s2 > top {
		top = s2
	}
	for _, b := range []uint64{s1, s2} {
		if err := e.plantFinalized(anvil, b); err != nil {
			t.Fatal(err)
		}
	}
	if err := pollUntil(ctx, 10*time.Minute, func() bool {
		return !strings.Contains(state("msr"), "0-1") && !strings.Contains(state("msr"), "0-2") && (state("vb-0-usdc") == "" || state("vb-0-usdc") == "0")
	}); err != nil {
		t.Fatalf("sweeps not confirmed: msr=%q usdc book %q\nbot:\n%s", state("msr"), state("vb-0-usdc"), botLogs())
	}
	if got, _ := anvil.ERC20BalanceOf(ctx, usdc, vault1); got.Cmp(usd(25)) != 0 {
		t.Fatalf("generation 1 holds %s USDC units, want 25000000", got)
	}
	t.Logf("(7) PASS: ETH and USDC swept to generation 1 and confirmed")

	// (8) The hand-off: the queued withdrawal goes from generation 1, signed by
	// its key. The window ends when it is on L1.
	var w2Block uint64
	if err := pollUntil(ctx, 20*time.Minute, func() bool {
		_, n, found, _ := anvil.FindTxFrom(ctx, vault1, 0, top)
		w2Block = n
		return found
	}); err != nil {
		t.Fatalf("generation 1 never sent the queued withdrawal\nbot:\n%s", botLogs())
	}
	window := devnetSince(t0)
	if got, _ := anvil.cli.BalanceAt(ctx, recip2, nil); got.Sign() <= 0 {
		t.Fatal("recipient 2 not paid")
	}
	if err := e.plantFinalized(anvil, w2Block); err != nil {
		t.Fatal(err)
	}
	if err := pollUntil(ctx, 8*time.Minute, func() bool { return state("n") == "1" && state("d-0") == "" && state("wnh") == "" }); err != nil {
		t.Fatalf("generation 1's first withdrawal not settled: n=%q wnh=%q\nbot:\n%s", state("n"), state("wnh"), botLogs())
	}
	t.Logf("(8) PASS: first withdrawal from generation 1 (nonce 0) sent %s after activateKey (devnet: instant finality, headers planted at once)", window.Round(time.Second))

	// (9) The kept-back gas and the stray ETH go in a leftover sweep; then the
	// generation retires and, 64 L1 blocks later, is purged.
	// The leftover sweep's header is planted once: an L1 call every Hive
	// block loses every L2 slot (SLOT-BOUNDARY-TX-1; run 4 halted on it).
	planted := false
	if err := pollUntil(ctx, 20*time.Minute, func() bool {
		_, n, found, _ := anvil.FindTxFrom(ctx, vault0, 3, top)
		if found && !planted {
			planted = e.plantFinalized(anvil, n) == nil
		}
		return found && (state("msr") == "")
	}); err != nil {
		t.Fatalf("leftover sweep not done: msr=%q eth book %q\nbot:\n%s", state("msr"), state("vb-0-eth"), botLogs())
	}
	if err := pollUntil(ctx, 10*time.Minute, func() bool { return strings.Contains(botLogs(), "inactivated=0") }); err != nil {
		t.Fatalf("generation 0 not retired\nbot:\n%s", botLogs())
	}
	_ = anvil.Mine(ctx, 70)
	head, _ := anvil.cli.BlockNumber(ctx)
	if err := e.plantFinalized(anvil, head); err != nil {
		t.Fatal(err)
	}
	if err := pollUntil(ctx, 10*time.Minute, func() bool { return strings.Contains(botLogs(), "purged=0") }); err != nil {
		t.Fatalf("generation 0 not purged\nbot:\n%s", botLogs())
	}
	left, _ := anvil.cli.BalanceAt(ctx, vault0, nil)
	t.Logf("(9) PASS: generation 0 retired and purged; %s wei left at its address", left)

	// End state: custody is at generation 1; the books are empty.
	bal1, _ := anvil.cli.BalanceAt(ctx, vault1, nil)
	sEth := state("s-eth")
	t.Logf("END: generation 1 holds %s wei and 25 USDC; s-eth=%s; vb-0-eth=%q", bal1, sEth, state("vb-0-eth"))
	if active, _ := new(big.Int).SetString(strings.Split(sEth, "|")[0], 10); active == nil || bal1.Cmp(active) < 0 {
		t.Fatalf("generation 1 holds %s wei, below the ETH custody %s", bal1, sEth)
	}
	if left.Cmp(big.NewInt(1e15)) > 0 {
		t.Fatalf("%s wei left at generation 0 after its purge", left)
	}
}
