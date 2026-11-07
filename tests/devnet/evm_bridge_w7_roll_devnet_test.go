//go:build evm_devnet

package devnet

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vsc-node/lib/dids"

	ethtypes "github.com/ethereum/go-ethereum/core/types"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

// W7 (account-mapping D3): a withdrawal that never mines is rolled back.
//
// unmapETH queues 1 ETH and nobody broadcasts it (no bot runs). rollWithdrawal
// is refused while the verified L1 chain is inside the 1200-block window; past
// it a third party rolls the withdrawal. The bot, started then, broadcasts the
// TSS-signed 0-value vault self-transfer at the withdrawal's nonce and confirms
// it, which refunds the withdrawer. The original withdrawal, TSS-signed at
// unmap, can then never mine.
func TestEVMBridge_W7_StuckWithdrawalRolledBack(t *testing.T) {
	if testing.Short() {
		t.Skip("devnet")
	}
	t.Parallel()
	requireDocker(t)

	ctx, cancel := devnetWithTimeout(context.Background(), 70*time.Minute)
	t.Cleanup(cancel)

	e := setupEvmBridgeDevnet(t, ctx)
	anvil := StartAnvil(t, ctx, evmChainID, nextAnvilPort())
	t.Cleanup(anvil.Stop)
	eth := func(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(1e18)) }

	if err := e.proposeExecute("registerRelayer", fmt.Sprintf(`{"account":"%s"}`, e.ownerAcct)); err != nil {
		t.Fatalf("registerRelayer: %v", err)
	}
	vault, err := e.tssVault(25 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := anvil.SetBalance(ctx, vault, eth(100)); err != nil {
		t.Fatalf("fund vault gas: %v", err)
	}

	// Depositor K: 6 ETH in (the 1% tax funds the gas reserve), 1 ETH out.
	depKey, _ := ethCrypto.GenerateKey()
	did, err := e.depositETH(anvil, depKey, vault, eth(6))
	if err != nil {
		t.Fatalf("deposit: %v", err)
	}
	if err := e.fundDID(did, "10000.000"); err != nil {
		t.Fatalf("fund K DID for RC: %v", err)
	}
	K, err := NewEthKey(hex.EncodeToString(ethCrypto.FromECDSA(depKey)))
	if err != nil {
		t.Fatal(err)
	}
	recipKey, _ := ethCrypto.GenerateKey()
	recip := ethCrypto.PubkeyToAddress(recipKey.PublicKey)
	if _, err := e.d.CallContractEvm(ctx, e.gqlNode, K, e.amID, "unmapETH",
		fmt.Sprintf(`{"to":"%s","amount":"1000000000000000000","asset":"eth"}`, recip.Hex()), 5000000); err != nil {
		t.Fatalf("unmapETH: %v", err)
	}
	type pendingSpend struct {
		Amount        *big.Int `json:"amount"`
		To            string   `json:"to"`
		Asset         string   `json:"asset"`
		UnsignedTxHex string   `json:"unsigned_tx_hex"`
		BlockHeight   uint64   `json:"block_height"`
		Rolled        bool     `json:"rolled"`
	}
	readPS := func() (pendingSpend, bool) {
		var ps pendingSpend
		s := e.stateStr("d-0")
		return ps, s != "" && json.Unmarshal([]byte(s), &ps) == nil && ps.UnsignedTxHex != ""
	}
	var orig pendingSpend
	if err := pollUntil(ctx, evmL2Wait, func() bool { var ok bool; orig, ok = readPS(); return ok }); err != nil {
		t.Fatal("unmapETH queued no withdrawal")
	}
	balKey := "a-" + did + "-eth"
	afterUnmap := readIntState(t, e, ctx, balKey)
	t.Logf("queued: 1 ETH to %s at L1 block %d; withdrawer balance %d", recip.Hex(), orig.BlockHeight, afterUnmap)

	// The roller is a third party: a funded EVM identity that is not the withdrawer.
	// Its calls carry a 5M RC limit, which the node checks against the RC the
	// HBD buys (1 HBD = 1000 RC) before running them.
	strangerKey, _ := ethCrypto.GenerateKey()
	S, _ := NewEthKey(hex.EncodeToString(ethCrypto.FromECDSA(strangerKey)))
	if err := e.fundDID(S.DID.String(), "10000.000"); err != nil {
		t.Fatalf("fund stranger DID: %v", err)
	}
	roll := func() (bool, string) {
		tx, err := e.d.CallContractEvm(ctx, e.gqlNode, S, e.amID, "rollWithdrawal", `{"nonce":0}`, 5000000)
		if err != nil {
			t.Fatalf("rollWithdrawal broadcast: %v", err)
		}
		return e.outResult(ctx, tx)
	}

	// (1) Inside the window: refused.
	if ok, msg := roll(); ok || !strings.Contains(msg, "can be rolled from L1 block") {
		t.Fatalf("(1) roll inside the window: ok=%v %q, want refused", ok, msg)
	}
	t.Logf("(1) PASS: roll refused inside the 1200-block window")

	// (2) Move the verified L1 chain past the window, then roll.
	if err := anvil.Mine(ctx, 1210); err != nil {
		t.Fatal(err)
	}
	fin, err := anvil.Finalized(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.plantFinalized(anvil, fin); err != nil {
		t.Fatal(err)
	}
	if fin < orig.BlockHeight+1200 {
		t.Fatalf("planted L1 block %d is not past the window (queued at %d)", fin, orig.BlockHeight)
	}
	if ok, msg := roll(); !ok {
		t.Fatalf("(2) roll past the window refused: %s", msg)
	}
	var rolled pendingSpend
	if err := pollUntil(ctx, evmL2Wait, func() bool { var ok bool; rolled, ok = readPS(); return ok && rolled.Rolled }); err != nil {
		t.Fatal("(2) the withdrawal is not marked rolled")
	}
	t.Logf("(2) PASS: a third party rolled the withdrawal past the window")

	// (3) The bot, started only now (the withdrawal was never broadcast), finds
	// the rolled spend, broadcasts the TSS-signed roll and confirms it; the
	// confirmSpend refunds the withdrawer. Its checkpoint starts past the
	// deposit, so it does not re-map it.
	botKey, _ := ethCrypto.GenerateKey()
	if err := e.fundDID(dids.NewEthDID(ethCrypto.PubkeyToAddress(botKey.PublicKey).Hex()).String(), "20000.000"); err != nil {
		t.Fatalf("fund bot DID for RC: %v", err)
	}
	cpPath := filepath.Join(t.TempDir(), "evm-bot-checkpoint.json")
	if err := os.WriteFile(cpPath, []byte(fmt.Sprintf(`{"version":2,"last_scanned_block":%d,"sent_withdrawals":{}}`, fin)), 0600); err != nil {
		t.Fatal(err)
	}
	bot := StartBot(t, ctx, BotConfig{EthRPC: anvil.URL, ContractID: e.amID, GraphQLURL: e.d.GQLEndpoint(e.gqlNode),
		EthPrivHex: hex.EncodeToString(ethCrypto.FromECDSA(botKey)), Checkpoint: cpPath})
	t.Cleanup(bot.Stop)
	botLogs := func() string { return strings.Join(bot.Logs(), "\n") }
	var rollTx *ethtypes.Transaction
	var blockN uint64
	if err := pollUntil(ctx, 20*time.Minute, func() bool {
		tx, n, found, ferr := anvil.FindTxFrom(ctx, vault, 0, fin)
		if ferr != nil || !found {
			return false
		}
		rollTx, blockN = tx, n
		return true
	}); err != nil {
		t.Fatalf("(3) the bot never broadcast the roll\nbot:\n%s", botLogs())
	}
	if rollTx.To() == nil || *rollTx.To() != vault || rollTx.Value().Sign() != 0 {
		t.Fatalf("(3) the vault sent to=%v value=%s at nonce 0, want the 0-value self-transfer", rollTx.To(), rollTx.Value())
	}
	if err := e.plantFinalized(anvil, blockN); err != nil {
		t.Fatal(err)
	}
	if err := pollUntil(ctx, 6*time.Minute, func() bool { return e.stateStr("n") == "1" && e.stateStr("d-0") == "" }); err != nil {
		t.Fatalf("(3) the bot never settled the roll: n=%q d-0 present=%v\nbot:\n%s", e.stateStr("n"), e.stateStr("d-0") != "", botLogs())
	}
	if got := readIntState(t, e, ctx, balKey); got != afterUnmap+1e18 {
		t.Fatalf("(3) withdrawer balance %d, want %d (the 1 ETH back)", got, afterUnmap+1e18)
	}
	if got, _ := anvil.cli.BalanceAt(ctx, recip, nil); got.Sign() != 0 {
		t.Fatalf("(3) the recipient holds %s wei on L1, want 0", got)
	}
	t.Logf("(3) PASS: the bot broadcast the roll (block %d) and its confirmSpend refunded 1 ETH", blockN)

	// (4) The original withdrawal was TSS-signed at unmap; it can never mine now.
	origTx, err := e.tssSign(orig.UnsignedTxHex, vault, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := origTx.MarshalBinary()
	if _, _, err := anvil.SendRawTx(ctx, raw); err == nil || !strings.Contains(strings.ToLower(err.Error()), "nonce") {
		t.Fatalf("(4) the original withdrawal broadcast after the roll: err=%v, want a nonce refusal", err)
	}
	if bal, _ := anvil.cli.BalanceAt(ctx, recip, nil); bal.Sign() != 0 {
		t.Fatalf("(4) the recipient was paid %s wei on L1 after the refund", bal)
	}
	t.Logf("(4) PASS: the original withdrawal is refused at nonce 0; the recipient got nothing on L1")
}
