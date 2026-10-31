//go:build evm_devnet

// WUSDC: USDC through the whole EVM bridge path, as W5 does for ETH, with a
// MockUSDC on anvil that keeps Circle's transfer rules (pause, blacklist, its
// own address blacklisted). The token is registered through propose/execute
// (a second token under the same symbol is refused, M15-N5); the real
// evm-mapping-bot, told the token through TOKENS, maps a USDC deposit; a
// user's unmapERC20 is signed by the TSS committee, broadcast by the bot and
// settled by its confirmSpend. A withdrawal to a blacklisted address reverts
// on L1 and is refunded while the gas reserve pays (USDC-1). The bot's /health
// is read throughout: no custody alarm on the bridge's real state, an alarm
// once the vault is short.
//
// Run: go test -tags evm_devnet -run TestEVMBridge_WUSDC ./tests/devnet/ -timeout 110m -v
package devnet

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"vsc-node/lib/dids"

	ethcommon "github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

func TestEVMBridge_WUSDC_BotEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("devnet")
	}
	t.Parallel()
	ctx, cancel := devnetWithTimeout(context.Background(), 100*time.Minute)
	t.Cleanup(cancel)

	e := setupEvmBridgeDevnet(t, ctx)
	anvil := StartAnvil(t, ctx, evmChainID, nextAnvilPort())
	t.Cleanup(anvil.Stop)
	eth := func(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(1e18)) }
	usd := func(n int64) *big.Int { return big.NewInt(n * 1_000_000) }
	regKey := func(a ethcommon.Address) string {
		return "t-" + strconv.FormatUint(evmChainID, 10) + "-" + strings.ToLower(a.Hex()[2:])
	}

	// L1: USDC, and a second token to try registering under its symbol.
	ownerKey := mustKey(t)
	if err := anvil.SetBalance(ctx, cryptoPubkeyAddr(ownerKey), eth(100)); err != nil {
		t.Fatal(err)
	}
	usdc, err := anvil.DeployMockUSDC(ctx, ownerKey)
	if err != nil {
		t.Fatal(err)
	}
	fake, err := anvil.DeployMockUSDC(ctx, ownerKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("MockUSDC %s, second token %s", usdc.Hex(), fake.Hex())

	// (1) registerToken usdc; then the second token under usdc (M15-N5).
	reg := func(a ethcommon.Address) string {
		return fmt.Sprintf(`{"address":"%s","symbol":"usdc","decimals":6,"min_withdrawal":1000000}`, a.Hex())
	}
	if err := e.proposeExecute("registerToken", reg(usdc)); err != nil {
		t.Fatalf("registerToken usdc: %v", err)
	}
	if err := pollUntil(ctx, evmL2Wait, func() bool { return e.stateStr(regKey(usdc)) == "usdc|6|1000000" }); err != nil {
		t.Fatalf("registerToken not stored: %s=%q", regKey(usdc), e.stateStr(regKey(usdc)))
	}
	ok, em := e.proposeExecuteResult("registerToken", reg(fake))
	if ok || !strings.Contains(em, "symbol usdc belongs to token") {
		t.Fatalf("a second token under usdc: ok=%v %q", ok, em)
	}
	if got := e.stateStr(regKey(fake)); got != "" {
		t.Fatalf("the refused token is in the registry: %s=%q", regKey(fake), got)
	}
	t.Logf("(1) PASS: usdc registered for %s; a second token under usdc refused: %s", usdc.Hex(), em)

	// The bot is the relayer for ETH deposits, as in W5, and watches USDC.
	botKey := mustKey(t)
	botDID := dids.NewEthDID(ethCrypto.PubkeyToAddress(botKey.PublicKey).Hex()).String()
	if err := e.proposeExecute("registerRelayer", fmt.Sprintf(`{"account":"%s"}`, botDID)); err != nil {
		t.Fatalf("registerRelayer: %v", err)
	}
	if err := e.fundDID(botDID, "20000.000"); err != nil {
		t.Fatalf("fund bot DID for RC: %v", err)
	}
	e.botAttests = true
	vault, err := e.tssVault(25 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := anvil.SetBalance(ctx, vault, eth(10)); err != nil { // L1 gas for the withdrawals
		t.Fatalf("fund vault gas: %v", err)
	}
	port, err := freeLocalPort()
	if err != nil {
		t.Fatal(err)
	}
	healthAddr := "127.0.0.1:" + port
	bot := StartBot(t, ctx, BotConfig{
		EthRPC: anvil.URL, ContractID: e.amID, GraphQLURL: e.d.GQLEndpoint(e.gqlNode),
		EthPrivHex: hex.EncodeToString(ethCrypto.FromECDSA(botKey)), Tokens: usdc.Hex(), HealthAddr: healthAddr,
	})
	t.Cleanup(bot.Stop)
	botLogs := func() string { return strings.Join(bot.Logs(), "\n") }
	if !bot.WaitForLog(ctx, "E23: attested the vault key's check signature", evmL2Wait+10*time.Minute) {
		t.Fatalf("the bot never attested the vault key's check signature:\n%s", botLogs())
	}
	if !bot.WaitForLog(ctx, "A18: vault is the TSS key's address", evmL2Wait) {
		t.Fatalf("the bot did not take the vault after the attest:\n%s", botLogs())
	}
	t.Logf("vault %s live; bot watches %s, /health on %s", vault.Hex(), usdc.Hex(), healthAddr)

	// /health samples, every 3 s until the alarms phase.
	type sample struct {
		at time.Time
		h  BotHealth
	}
	var (
		smu     sync.Mutex
		samples []sample
	)
	sctx, stopSampling := context.WithCancel(ctx)
	t.Cleanup(stopSampling)
	go func() {
		for sctx.Err() == nil {
			if h, err := bot.Health(sctx, healthAddr); err == nil {
				if at, perr := time.Parse(time.RFC3339, h.CheckedAt); perr == nil {
					smu.Lock()
					samples = append(samples, sample{at, *h})
					smu.Unlock()
				}
			}
			select {
			case <-sctx.Done():
			case <-devnetAfter(3 * time.Second):
			}
		}
	}()
	c4 := func(h BotHealth) []string {
		var out []string
		for _, i := range h.Issues {
			if strings.HasPrefix(i, "C4") {
				out = append(out, i)
			}
		}
		return out
	}
	// freshHealth waits for a /health check made after `after` that satisfies ok.
	freshHealth := func(after time.Time, ok func(BotHealth) bool) (BotHealth, error) {
		var last BotHealth
		err := pollUntil(ctx, 3*time.Minute, func() bool {
			h, err := bot.Health(ctx, healthAddr)
			if err != nil {
				return false
			}
			at, perr := time.Parse(time.RFC3339, h.CheckedAt)
			last = *h
			return perr == nil && at.After(after) && ok(*h)
		})
		return last, err
	}

	// (2) Deposits: 6 ETH (1% to the gas reserve, as in W5) and 25 USDC.
	depKey := mustKey(t)
	depAddr := ethCrypto.PubkeyToAddress(depKey.PublicKey)
	user := evmDID(depAddr)
	if err := anvil.SetBalance(ctx, depAddr, eth(7)); err != nil {
		t.Fatal(err)
	}
	_, ethBlock, err := anvil.SendETH(ctx, depKey, vault, eth(6))
	if err != nil {
		t.Fatalf("ETH deposit: %v", err)
	}
	if err := e.plantFinalized(anvil, ethBlock); err != nil {
		t.Fatal(err)
	}
	if err := pollUntil(ctx, 6*time.Minute, func() bool { return e.stateStr("gr") == "60000000000000000" }); err != nil {
		t.Fatalf("the ETH deposit never seeded the reserve: gr=%q\nbot:\n%s", e.stateStr("gr"), botLogs())
	}
	if rc, err := anvil.SendTx(ctx, ownerKey, &usdc, usdcCall("mint(address,uint256)", depAddr, usd(30)), 100_000); err != nil || rc.Status != 1 {
		t.Fatalf("mint: %v %+v", err, rc)
	}
	rc, err := anvil.SendTx(ctx, depKey, &usdc, usdcCall("transfer(address,uint256)", vault, usd(25)), 100_000)
	if err != nil || rc.Status != 1 {
		t.Fatalf("USDC deposit transfer: %v %+v", err, rc)
	}
	if err := e.plantFinalized(anvil, rc.BlockNumber.Uint64()); err != nil {
		t.Fatal(err)
	}
	usdcBal := "a-" + user + "-usdc"
	if err := pollUntil(ctx, 6*time.Minute, func() bool { return e.stateStr(usdcBal) == "25000000" }); err != nil {
		t.Fatalf("the bot never mapped the USDC deposit: %s=%q\nbot:\n%s", usdcBal, e.stateStr(usdcBal), botLogs())
	}
	if got := e.stateStr("s-usdc"); got != "25000000|25000000|0|0" {
		t.Fatalf("s-usdc=%q after the deposit", got)
	}
	mapped := time.Now() // wall: compared with the bot's /health checkedAt, its own wall clock
	if h, err := freshHealth(mapped, func(h BotHealth) bool { return true }); err != nil || len(c4(h)) != 0 {
		t.Fatalf("/health after the deposits: %v %+v", err, h)
	}
	t.Logf("(2) PASS: the bot mapped 25 USDC (and the 6 ETH that seeds the reserve); /health has no custody alarm")

	// (3) An L2 transfer of usdc (the E1 asset rule on the deployed bridge).
	if err := e.fundDID(user, "10000.000"); err != nil {
		t.Fatalf("fund user DID for RC: %v", err)
	}
	K, err := NewEthKey(hex.EncodeToString(ethCrypto.FromECDSA(depKey)))
	if err != nil {
		t.Fatal(err)
	}
	other := evmDID(ethCrypto.PubkeyToAddress(mustKey(t).PublicKey))
	if _, err := e.d.CallContractEvm(ctx, e.gqlNode, K, e.amID, "transfer", fmt.Sprintf(`{"to":"%s","amount":"5000000","asset":"usdc"}`, other), 5000000); err != nil {
		t.Fatalf("transfer: %v", err)
	}
	if err := pollUntil(ctx, evmL2Wait, func() bool { return e.stateStr("a-"+other+"-usdc") == "5000000" }); err != nil {
		t.Fatalf("L2 usdc transfer not applied: %q", e.stateStr("a-"+other+"-usdc"))
	}
	t.Logf("(3) PASS: 5 usdc moved on L2 to %s", other)

	// withdraw queues an unmapERC20 at nonce and returns the queued tx.
	type pending struct {
		Nonce         uint64   `json:"nonce"`
		Amount        *big.Int `json:"amount"`
		UnsignedTxHex string   `json:"unsigned_tx_hex"`
	}
	withdraw := func(to ethcommon.Address, amount *big.Int, nonce uint64) (*ethtypes.DynamicFeeTx, time.Time) {
		t.Helper()
		payload := fmt.Sprintf(`{"to":"%s","amount":"%s","token_address":"%s"}`, to.Hex(), amount, usdc.Hex())
		if _, err := e.d.CallContractEvm(ctx, e.gqlNode, K, e.amID, "unmapERC20", payload, 5000000); err != nil {
			t.Fatalf("unmapERC20: %v", err)
		}
		key := "d-" + strconv.FormatUint(nonce, 10)
		var ps pending
		if err := pollUntil(ctx, evmL2Wait, func() bool {
			return json.Unmarshal([]byte(e.stateStr(key)), &ps) == nil && ps.UnsignedTxHex != ""
		}); err != nil {
			t.Fatalf("unmapERC20 queued nothing at %s (%q)", key, e.stateStr(key))
		}
		q, err := DecodeUnsigned1559(ps.UnsignedTxHex)
		if err != nil {
			t.Fatal(err)
		}
		if q.To == nil || *q.To != usdc || q.Value.Sign() != 0 || !strings.EqualFold(hex.EncodeToString(q.Data), hex.EncodeToString(usdcCall("transfer(address,uint256)", to, amount))) {
			t.Fatalf("queued tx to=%v value=%s data=%x, want transfer(%s, %s) on %s", q.To, q.Value, q.Data, to.Hex(), amount, usdc.Hex())
		}
		return q, time.Now() // wall: compared with the bot's /health checkedAt, its own wall clock
	}
	// sent waits for the vault's L1 tx at nonce and returns it with its receipt.
	sent := func(nonce, from uint64) (*ethtypes.Transaction, *ethtypes.Receipt) {
		t.Helper()
		var tx *ethtypes.Transaction
		if err := pollUntil(ctx, 20*time.Minute, func() bool {
			got, _, found, ferr := anvil.FindTxFrom(ctx, vault, nonce, from)
			tx = got
			return ferr == nil && found
		}); err != nil {
			t.Fatalf("the vault never sent nonce %d on L1\nbot:\n%s", nonce, botLogs())
		}
		rc, err := anvil.cli.TransactionReceipt(ctx, tx.Hash())
		if err != nil {
			t.Fatal(err)
		}
		return tx, rc
	}

	// (4) Withdraw 12 USDC: custody and the reserve move at unmap; the TSS
	// signature is broadcast and mines; the bot's confirmSpend closes it.
	recip := ethCrypto.PubkeyToAddress(mustKey(t).PublicKey)
	gr0, _ := new(big.Int).SetString(e.stateStr("gr"), 10)
	q0, queued0 := withdraw(recip, usd(12), 0)
	gr1, _ := new(big.Int).SetString(e.stateStr("gr"), 10)
	if gr1 == nil || gr1.Cmp(gr0) >= 0 {
		t.Fatalf("gas reserve %s -> %s at unmapERC20, want it lower", gr0, gr1)
	}
	if got := e.stateStr("s-usdc"); got != "13000000|13000000|0|0" {
		t.Fatalf("s-usdc=%q after queuing 12 USDC", got)
	}
	tx0, rc0 := sent(0, ethBlock)
	mined0 := time.Now() // wall: compared with the bot's /health checkedAt, its own wall clock
	if rc0.Status != 1 || tx0.Nonce() != q0.Nonce || tx0.Gas() != q0.Gas {
		t.Fatalf("withdrawal tx status=%d nonce=%d gas=%d, queued nonce=%d gas=%d", rc0.Status, tx0.Nonce(), tx0.Gas(), q0.Nonce, q0.Gas)
	}
	if b, _ := anvil.ERC20BalanceOf(ctx, usdc, recip); b.Cmp(usd(12)) != 0 {
		t.Fatalf("recipient holds %s USDC units, want 12000000", b)
	}
	if err := e.plantFinalized(anvil, rc0.BlockNumber.Uint64()); err != nil {
		t.Fatal(err)
	}
	if err := pollUntil(ctx, 6*time.Minute, func() bool { return e.stateStr("n") == "1" && e.stateStr("d-0") == "" }); err != nil {
		t.Fatalf("the bot never settled the withdrawal: n=%q\nbot:\n%s", e.stateStr("n"), botLogs())
	}
	if got := e.stateStr(usdcBal); got != "8000000" {
		t.Fatalf("user usdc %q after the withdrawal, want 8000000", got)
	}
	t.Logf("(4) PASS: 12 USDC withdrawn (L1 gas used %d of %d), settled by the bot; the reserve paid %s wei (estimate)", rc0.GasUsed, tx0.Gas(), new(big.Int).Sub(gr0, gr1))

	// (5) USDC-1: 8 USDC to the token's own address, which it blacklists. The
	// transfer mines and reverts; confirmSpend refunds the user; the reserve
	// and the vault's ETH paid for it.
	vaultETH, _ := anvil.cli.BalanceAt(ctx, vault, nil)
	q1, queued1 := withdraw(usdc, usd(8), 1)
	gr2, _ := new(big.Int).SetString(e.stateStr("gr"), 10)
	tx1, rc1 := sent(1, rc0.BlockNumber.Uint64())
	mined1 := time.Now() // wall: compared with the bot's /health checkedAt, its own wall clock
	if rc1.Status != 0 || tx1.Nonce() != q1.Nonce {
		t.Fatalf("withdrawal to the blacklisted address: status=%d nonce=%d, want a mined revert at %d", rc1.Status, tx1.Nonce(), q1.Nonce)
	}
	if b, _ := anvil.ERC20BalanceOf(ctx, usdc, vault); b.Cmp(usd(13)) != 0 {
		t.Fatalf("vault holds %s USDC units after the revert, want 13000000", b)
	}
	if err := e.plantFinalized(anvil, rc1.BlockNumber.Uint64()); err != nil {
		t.Fatal(err)
	}
	if err := pollUntil(ctx, 6*time.Minute, func() bool { return e.stateStr("n") == "2" && e.stateStr("d-1") == "" }); err != nil {
		t.Fatalf("the bot never settled the reverted withdrawal: n=%q\nbot:\n%s", e.stateStr("n"), botLogs())
	}
	if got := e.stateStr(usdcBal); got != "8000000" {
		t.Fatalf("user usdc %q after the reverted withdrawal, want the 8000000 back", got)
	}
	if got := e.stateStr("s-usdc"); got != "13000000|13000000|0|0" {
		t.Fatalf("s-usdc=%q after the refund", got)
	}
	if got := e.stateStr("gr"); got != gr2.String() {
		t.Fatalf("gas reserve %s after the refund, want it to stay at %s", got, gr2)
	}
	vaultETH2, _ := anvil.cli.BalanceAt(ctx, vault, nil)
	paid := new(big.Int).Mul(new(big.Int).SetUint64(rc1.GasUsed), rc1.EffectiveGasPrice)
	if spent := new(big.Int).Sub(vaultETH, vaultETH2); spent.Cmp(paid) != 0 {
		t.Fatalf("vault ETH fell by %s wei, want the reverted tx's %s", spent, paid)
	}
	settled := time.Now() // wall: compared with the bot's /health checkedAt, its own wall clock
	t.Logf("(5) PASS (USDC-1): the reverted withdrawal was refunded; the reserve paid %s wei (estimate), the vault %s wei (gas used %d)",
		new(big.Int).Sub(gr1, gr2), paid, rc1.GasUsed)

	// No custody alarm in any /health check through both withdrawals; report
	// how many checks fell while a withdrawal was queued but not yet mined
	// (custody already lowered, the vault not yet paid out).
	stopSampling()
	smu.Lock()
	var inFlight, total int
	for _, s := range samples {
		if !s.at.Before(mapped) && !s.at.After(settled) {
			total++
			if bad := c4(s.h); len(bad) != 0 {
				smu.Unlock()
				t.Fatalf("custody alarm at %s with the bridge's real state: %v", s.at, bad)
			}
			if (s.at.After(queued0) && s.at.Before(mined0)) || (s.at.After(queued1) && s.at.Before(mined1)) {
				inFlight++
			}
		}
	}
	smu.Unlock()
	if total == 0 {
		t.Fatal("no /health check between the deposits and the last settlement")
	}
	t.Logf("(6) PASS: %d /health checks from the deposits to the last settlement, %d while a withdrawal was queued and not yet mined: no custody alarm", total, inFlight)

	// (7) The vault one unit short of USDC custody, then of ETH custody: alarms.
	if err := anvil.SetMockUSDCBalance(ctx, usdc, vault, new(big.Int).Sub(usd(13), big.NewInt(1))); err != nil {
		t.Fatal(err)
	}
	cut := time.Now() // wall: compared with the bot's /health checkedAt, its own wall clock
	want := "holds 12999999 usdc, 1 short of the 13000000 usdc"
	if h, err := freshHealth(cut, func(h BotHealth) bool { return len(c4(h)) > 0 }); err != nil || !strings.Contains(strings.Join(c4(h), "\n"), want) {
		t.Fatalf("/health with the vault 1 USDC unit short: %v %+v, want %q", err, h, want)
	}
	ethCustody := strings.Split(e.stateStr("s-eth"), "|")[0]
	custody, ok := new(big.Int).SetString(ethCustody, 10)
	if !ok {
		t.Fatalf("s-eth=%q", e.stateStr("s-eth"))
	}
	if err := anvil.SetBalance(ctx, vault, new(big.Int).Sub(custody, big.NewInt(1))); err != nil {
		t.Fatal(err)
	}
	cut = time.Now() // wall: compared with the bot's /health checkedAt, its own wall clock
	want = "wei, 1 short of the " + custody.String() + " wei of ETH"
	if h, err := freshHealth(cut, func(h BotHealth) bool { return strings.Contains(strings.Join(c4(h), "\n"), "wei of ETH") }); err != nil || !strings.Contains(strings.Join(c4(h), "\n"), want) {
		t.Fatalf("/health with the vault 1 wei short of ETH custody: %v %+v, want %q", err, h, want)
	}
	t.Logf("(7) PASS: /health reports the vault 1 USDC unit short, and 1 wei short of ETH custody")
}
