//go:build evm_devnet

// W5: the whole EVM bridge path, with only the ZK proof stood in. TSS keygen
// gives the vault address; the real evm-mapping-bot maps a deposit made on
// anvil; a user's unmapETH is signed by the TSS committee; the bot broadcasts
// that signature to anvil and settles it with confirmSpend. The mock verifier
// only plants the L1 headers (real roots) a prover would deliver.
//
// Run: go test -tags evm_devnet -run TestEVMBridge_W5 ./tests/devnet/ -timeout 100m -v
package devnet

import (
	"bytes"
	"context"
	"crypto/ecdsa"
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

func TestEVMBridge_W5_BotEndToEnd(t *testing.T) {
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

	// The bot's L2 identity is the relayer that maps ETH deposits.
	botKey, _ := ethCrypto.GenerateKey()
	botDID := dids.NewEthDID(ethCrypto.PubkeyToAddress(botKey.PublicKey).Hex()).String()
	if err := e.proposeExecute("registerRelayer", fmt.Sprintf(`{"account":"%s"}`, botDID)); err != nil {
		t.Fatalf("registerRelayer: %v", err)
	}
	if err := e.fundDID(botDID, "20000.000"); err != nil {
		t.Fatalf("fund bot DID for RC: %v", err)
	}

	// The vault is the address of the contract's TSS primary key, once the key
	// has signed the vault check message (E23); here the bot does that itself.
	t.Logf("waiting for TSS primary keygen...")
	e.botAttests = true
	vault, err := e.tssVault(25 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := anvil.SetBalance(ctx, vault, eth(10)); err != nil { // L1 gas for the withdrawal
		t.Fatalf("fund vault gas: %v", err)
	}
	t.Logf("vault = TSS primary key address %s", vault.Hex())

	// No VAULT_ADDRESS: the bot reads the vault from the TSS key (A18).
	bot := StartBot(t, ctx, BotConfig{
		EthRPC: anvil.URL, ContractID: e.amID,
		GraphQLURL: e.d.GQLEndpoint(e.gqlNode), EthPrivHex: hex.EncodeToString(ethCrypto.FromECDSA(botKey)),
	})
	t.Cleanup(bot.Stop)
	botLogs := func() string { return strings.Join(bot.Logs(), "\n") }
	if !bot.WaitForLog(ctx, "evm-mapping-bot starting", time.Minute) {
		t.Fatalf("bot did not start:\n%s", botLogs())
	}
	// (0) E23: the bot asks the key to sign the vault check message and attests
	// the network's signature; only then is the address the vault.
	if !bot.WaitForLog(ctx, "E23: attested the vault key's check signature", evmL2Wait+10*time.Minute) {
		t.Fatalf("the bot never attested the vault key's check signature:\n%s", botLogs())
	}
	pub := strings.ToLower(strings.TrimPrefix(e.getTssPubKey(), "0x"))
	if err := pollUntil(ctx, evmL2Wait, func() bool { return e.stateStr("vaultok") == pub }); err != nil {
		t.Fatalf("vaultok=%q after the bot's attest, want %s", e.stateStr("vaultok"), pub)
	}
	if !bot.WaitForLog(ctx, "A18: vault is the TSS key's address", evmL2Wait) {
		t.Fatalf("the bot did not take the vault after the attest:\n%s", botLogs())
	}
	t.Logf("(0) PASS: the bot drove the vault check (E23); vault %s live", vault.Hex())

	// (1) Deposit 6 ETH on anvil; the prover stand-in plants its header; the
	// bot detects and maps it. 1% goes to the gas reserve.
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
	const credited = "5940000000000000000" // 6 ETH less the 1% reserve tax
	if err := pollUntil(ctx, 6*time.Minute, func() bool { return e.stateStr(balKey) == credited }); err != nil {
		t.Fatalf("bot never mapped the deposit: %s=%q, want %s\nbot:\n%s", balKey, e.stateStr(balKey), credited, botLogs())
	}
	if gr := e.stateStr("gr"); gr != "60000000000000000" {
		t.Fatalf("gas reserve gr=%q after the deposit, want 60000000000000000", gr)
	}
	t.Logf("(1) PASS: the bot mapped the anvil deposit; %s credited %s wei, reserve 0.06 ETH", user, credited)

	// (2) The user withdraws 1 ETH. The TSS committee signs the queued tx, the
	// bot broadcasts it, and once its header lands the bot confirms it.
	if err := e.fundDID(user, "10000.000"); err != nil {
		t.Fatalf("fund user DID for RC: %v", err)
	}
	K, err := NewEthKey(hex.EncodeToString(ethCrypto.FromECDSA(depKey)))
	if err != nil {
		t.Fatal(err)
	}
	recip := ethCrypto.PubkeyToAddress(mustKey(t).PublicKey)
	unmap := fmt.Sprintf(`{"to":"%s","amount":"1000000000000000000","asset":"eth"}`, recip.Hex())
	if _, err := e.d.CallContractEvm(ctx, e.gqlNode, K, e.amID, "unmapETH", unmap, 5000000); err != nil {
		t.Fatalf("unmapETH: %v", err)
	}
	var ps struct {
		Nonce         uint64   `json:"nonce"`
		Amount        *big.Int `json:"amount"`
		To            string   `json:"to"`
		UnsignedTxHex string   `json:"unsigned_tx_hex"`
	}
	if err := pollUntil(ctx, evmL2Wait, func() bool {
		return json.Unmarshal([]byte(e.stateStr("d-0")), &ps) == nil && ps.UnsignedTxHex != ""
	}); err != nil {
		t.Fatalf("unmapETH queued no withdrawal (d-0=%q)", e.stateStr("d-0"))
	}
	queued, err := DecodeUnsigned1559(ps.UnsignedTxHex)
	if err != nil {
		t.Fatal(err)
	}
	afterUnmap := e.stateStr(balKey)
	t.Logf("queued withdrawal nonce=%d value=%s to=%s; user balance now %s", ps.Nonce, queued.Value, queued.To.Hex(), afterUnmap)

	var mined *ethtypes.Transaction
	var wBlock uint64
	if err := pollUntil(ctx, 20*time.Minute, func() bool {
		tx, n, found, ferr := anvil.FindTxFrom(ctx, vault, ps.Nonce, depBlock)
		if ferr != nil || !found {
			return false
		}
		mined, wBlock = tx, n
		return true
	}); err != nil {
		t.Fatalf("the vault never sent the withdrawal on L1 (TSS signature or bot broadcast missing)\nbot:\n%s", botLogs())
	}
	if mined.To() == nil || *mined.To() != recip || mined.Value().Cmp(queued.Value) != 0 || mined.Nonce() != queued.Nonce ||
		mined.GasFeeCap().Cmp(queued.GasFeeCap) != 0 || mined.GasTipCap().Cmp(queued.GasTipCap) != 0 ||
		mined.Gas() != queued.Gas || !bytes.Equal(mined.Data(), queued.Data) {
		t.Fatalf("the L1 tx the vault sent differs from the queued withdrawal: sent to=%v value=%s nonce=%d, queued to=%v value=%s nonce=%d",
			mined.To(), mined.Value(), mined.Nonce(), queued.To, queued.Value, queued.Nonce)
	}
	if got, _ := anvil.cli.BalanceAt(ctx, recip, nil); got.Cmp(queued.Value) != 0 {
		t.Fatalf("recipient holds %s wei on L1, want %s", got, queued.Value)
	}
	t.Logf("(2a) PASS: TSS-signed withdrawal mined in block %d, recipient paid %s wei", wBlock, queued.Value)

	if err := e.plantFinalized(anvil, wBlock); err != nil {
		t.Fatalf("plant withdrawal header: %v", err)
	}
	if err := pollUntil(ctx, 6*time.Minute, func() bool { return e.stateStr("n") == "1" && e.stateStr("d-0") == "" }); err != nil {
		t.Fatalf("the bot never settled the withdrawal: n=%q d-0 present=%v\nbot:\n%s", e.stateStr("n"), e.stateStr("d-0") != "", botLogs())
	}
	if got := e.stateStr(balKey); got != afterUnmap {
		t.Fatalf("confirmSpend changed the user's balance %s -> %s", afterUnmap, got)
	}
	t.Logf("(2b) PASS: the bot's confirmSpend closed nonce 0 (n=1, d-0 cleared); user balance %s unchanged", afterUnmap)
}

func mustKey(t *testing.T) *ecdsa.PrivateKey {
	k, err := ethCrypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}
