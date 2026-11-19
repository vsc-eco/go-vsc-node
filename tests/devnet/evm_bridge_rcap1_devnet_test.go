//go:build evm_devnet

// CONFIRM-RECEIPT-CAP-1 on the devnet: a user withdraws ETH to a contract whose
// receive logs 10 KB, and then makes a normal withdrawal behind it. The
// withdrawal to the contract mines and pays, but a confirmSpend carrying its
// receipt is over the node's 8 KB contract payload cap. Without the fix the
// confirmed nonce never moves, and the withdrawal behind it waits for good.
// With it, the bot closes the withdrawal on its inclusion proof, notes that
// its receipt is too large to settle, and then confirms the next withdrawal.
//
// Run: go test -tags evm_devnet -run TestEVMBridge_RCAP1 ./tests/devnet/ -timeout 100m -v
package devnet

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"vsc-node/lib/dids"

	ethtypes "github.com/ethereum/go-ethereum/core/types"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

// rcap1LoggerCode is a contract's runtime code that logs 10,000 zero bytes
// (LOG0 of memory 0..10000) whenever it is called: PUSH2 0x2710, PUSH1 0,
// LOG0, STOP. About 82k gas.
const rcap1LoggerCode = "0x6127106000a000"

func TestEVMBridge_RCAP1_OversizedReceiptDoesNotFreezeTheQueue(t *testing.T) {
	if testing.Short() {
		t.Skip("devnet")
	}
	t.Parallel()
	ctx, cancel := devnetWithTimeout(context.Background(), 90*time.Minute)
	t.Cleanup(cancel)

	e := setupEvmBridgeDevnet(t, ctx)
	anvil := StartAnvil(t, ctx, evmChainID, nextAnvilPort())
	t.Cleanup(anvil.Stop)
	eth := func(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(1e18)) }

	botKey, _ := ethCrypto.GenerateKey()
	botDID := dids.NewEthDID(ethCrypto.PubkeyToAddress(botKey.PublicKey).Hex()).String()
	if err := e.proposeExecute("registerRelayer", fmt.Sprintf(`{"account":"%s"}`, botDID)); err != nil {
		t.Fatalf("registerRelayer: %v", err)
	}
	if err := e.fundDID(botDID, "20000.000"); err != nil {
		t.Fatalf("fund bot DID for RC: %v", err)
	}
	t.Logf("waiting for TSS primary keygen...")
	e.botAttests = true
	vault, err := e.tssVault(25 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := anvil.SetBalance(ctx, vault, eth(10)); err != nil {
		t.Fatalf("fund vault gas: %v", err)
	}
	bot := StartBot(t, ctx, BotConfig{
		EthRPC: anvil.URL, ContractID: e.amID,
		GraphQLURL: e.d.GQLEndpoint(e.gqlNode), EthPrivHex: hex.EncodeToString(ethCrypto.FromECDSA(botKey)),
	})
	t.Cleanup(bot.Stop)
	botLogs := func() string { return strings.Join(bot.Logs(), "\n") }
	if !bot.WaitForLog(ctx, "E23: attested the vault key's check signature", evmL2Wait+10*time.Minute) {
		t.Fatalf("the bot never attested the vault key's check signature:\n%s", botLogs())
	}
	if !bot.WaitForLog(ctx, "A18: vault is the TSS key's address", evmL2Wait) {
		t.Fatalf("the bot did not take the vault after the attest:\n%s", botLogs())
	}

	// (1) Deposit 6 ETH; the bot maps it.
	depKey, _ := ethCrypto.GenerateKey()
	depAddr := ethCrypto.PubkeyToAddress(depKey.PublicKey)
	if err := anvil.SetBalance(ctx, depAddr, eth(7)); err != nil {
		t.Fatal(err)
	}
	_, depBlock, err := anvil.SendETH(ctx, depKey, vault, eth(6))
	if err != nil {
		t.Fatalf("deposit send: %v", err)
	}
	if err := e.plantFinalized(anvil, depBlock); err != nil {
		t.Fatalf("plant deposit header: %v", err)
	}
	user := evmDID(depAddr)
	balKey := "a-" + user + "-eth"
	if err := pollUntil(ctx, 6*time.Minute, func() bool { return e.stateStr(balKey) == "5940000000000000000" }); err != nil {
		t.Fatalf("bot never mapped the deposit: %s=%q\nbot:\n%s", balKey, e.stateStr(balKey), botLogs())
	}
	t.Logf("(1) PASS: deposit mapped")

	// (2) The recipient: a contract that logs 10 KB on every call.
	logger := ethCrypto.PubkeyToAddress(mustKey(t).PublicKey)
	if err := anvil.rpc.CallContext(ctx, nil, "anvil_setCode", logger.Hex(), rcap1LoggerCode); err != nil {
		t.Fatalf("plant the logging recipient: %v", err)
	}
	if err := e.fundDID(user, "10000.000"); err != nil {
		t.Fatalf("fund user DID for RC: %v", err)
	}
	K, err := NewEthKey(hex.EncodeToString(ethCrypto.FromECDSA(depKey)))
	if err != nil {
		t.Fatal(err)
	}
	unmap := func(to string, gas uint64) {
		t.Helper()
		p := fmt.Sprintf(`{"to":"%s","amount":"1000000000000000000","asset":"eth","gas_limit":%d}`, to, gas)
		if _, err := e.d.CallContractEvm(ctx, e.gqlNode, K, e.amID, "unmapETH", p, 5000000); err != nil {
			t.Fatalf("unmapETH to %s: %v", to, err)
		}
	}
	unmap(logger.Hex(), 250_000)
	if err := pollUntil(ctx, evmL2Wait, func() bool { return e.stateStr("d-0") != "" }); err != nil {
		t.Fatal("the withdrawal to the logging contract was not queued")
	}
	recip := ethCrypto.PubkeyToAddress(mustKey(t).PublicKey)
	unmap(recip.Hex(), 21_000)
	if err := pollUntil(ctx, evmL2Wait, func() bool { return e.stateStr("d-1") != "" }); err != nil {
		t.Fatal("the withdrawal behind it was not queued")
	}
	t.Logf("(2) queued nonce 0 to the logging contract %s and nonce 1 to %s", logger.Hex(), recip.Hex())

	// (3) Nonce 0 mines, pays, and logs 10 KB.
	var mined *ethtypes.Transaction
	var wBlock uint64
	if err := pollUntil(ctx, 20*time.Minute, func() bool {
		tx, n, found, ferr := anvil.FindTxFrom(ctx, vault, 0, depBlock)
		if ferr != nil || !found {
			return false
		}
		mined, wBlock = tx, n
		return true
	}); err != nil {
		t.Fatalf("the vault never sent withdrawal 0\nbot:\n%s", botLogs())
	}
	rc, err := anvil.cli.TransactionReceipt(ctx, mined.Hash())
	if err != nil {
		t.Fatal(err)
	}
	if rc.Status != 1 || len(rc.Logs) != 1 || len(rc.Logs[0].Data) != 10_000 {
		t.Fatalf("fixture: withdrawal 0 status %d with %d logs, want a paid transfer with one 10 KB log", rc.Status, len(rc.Logs))
	}
	if got, _ := anvil.cli.BalanceAt(ctx, logger, nil); got.Cmp(eth(1)) != 0 {
		t.Fatalf("the logging contract holds %s wei, want 1 ETH", got)
	}
	idx, err := anvil.FindTxIndex(ctx, wBlock, mined.Hash())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("(3) PASS: withdrawal 0 mined in block %d at index %d, paid, logged 10 KB", wBlock, idx)

	// (4) Its confirm with the receipt does not fit: the queue must still move.
	if err := e.plantFinalized(anvil, wBlock); err != nil {
		t.Fatalf("plant withdrawal header: %v", err)
	}
	if err := pollUntil(ctx, 10*time.Minute, func() bool { return e.stateStr("n") != "" && e.stateStr("n") != "0" }); err != nil {
		t.Fatalf("FROZEN: withdrawal 0 was never confirmed (n=%q); every later withdrawal waits behind it\nbot:\n%s", e.stateStr("n"), botLogs())
	}
	if !strings.Contains(botLogs(), "closing the withdrawal on its inclusion proof") {
		t.Fatalf("n moved but the bot did not close on inclusion:\n%s", botLogs())
	}
	record := fmt.Sprintf("wic-%d-%d", wBlock, idx)
	var rec struct {
		Nonce  uint64   `json:"n"`
		Amount *big.Int `json:"a"`
	}
	if err := json.Unmarshal([]byte(e.stateStr(record)), &rec); err != nil || rec.Amount == nil || rec.Amount.Cmp(eth(1)) != 0 {
		t.Fatalf("inclusion-close record %s = %q", record, e.stateStr(record))
	}
	if !bot.WaitForLog(ctx, "the receipt is too large to settle", 5*time.Minute) {
		t.Fatalf("the bot did not give up on the unsettleable receipt:\n%s", botLogs())
	}
	t.Logf("(4) PASS: withdrawal 0 closed on its inclusion proof (n=%s, record %s); its receipt is too large to settle", e.stateStr("n"), record)

	// (5) The withdrawal behind it is confirmed as usual.
	var mined1 *ethtypes.Transaction
	var w1Block uint64
	if err := pollUntil(ctx, 20*time.Minute, func() bool {
		tx, n, found, ferr := anvil.FindTxFrom(ctx, vault, 1, wBlock)
		if ferr != nil || !found {
			return false
		}
		mined1, w1Block = tx, n
		return true
	}); err != nil {
		t.Fatalf("the vault never sent withdrawal 1\nbot:\n%s", botLogs())
	}
	if err := e.plantFinalized(anvil, w1Block); err != nil {
		t.Fatalf("plant withdrawal 1 header: %v", err)
	}
	if err := pollUntil(ctx, 10*time.Minute, func() bool { return e.stateStr("n") == "2" && e.stateStr("d-1") == "" }); err != nil {
		t.Fatalf("withdrawal 1 was never confirmed: n=%q\nbot:\n%s", e.stateStr("n"), botLogs())
	}
	if got, _ := anvil.cli.BalanceAt(ctx, recip, nil); got.Cmp(eth(1)) != 0 {
		t.Fatalf("recipient 1 holds %s wei, want 1 ETH", got)
	}
	t.Logf("(5) PASS: withdrawal 1 (%s) confirmed behind it; n=2", mined1.Hash().Hex())
}
