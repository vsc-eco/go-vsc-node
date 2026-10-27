//go:build evm_devnet

// W8: the 10-27 launch fixes through the real bot on a devnet, the mock
// verifier planting the L1 headers a prover would deliver:
//
//	RP-8  a native ETH deposit is mapped by a bot that is not a registered relayer
//	RP-7  MagiDeposit (deployed by CREATE2 for the vault) credits the recipient
//	      it names, for ETH and for USDC; the Transfer that paid the USDC is not
//	      also credited to the sender
//	RP-11 two users' withdrawals are in flight at once; the bot broadcasts both
//	      and confirms them in nonce order
//	GLAM-1 an unmap's gas_limit is the gas the vault's L1 tx carries
//
// Run: go test -tags evm_devnet -run TestEVMBridge_W8 ./tests/devnet/ -timeout 150m -v
package devnet

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"fmt"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"vsc-node/lib/dids"

	ethcommon "github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

// create2Deployer is the deterministic deployment proxy (anvil has it at genesis).
var create2Deployer = ethcommon.HexToAddress("0x4e59b44847b379578588920cA78FbF26c0B4956C")

// abiString appends abi-encoded head words then a dynamic string at the end.
func depositCalldata(sig string, static [][]byte, s string) []byte {
	out := ethCrypto.Keccak256([]byte(sig))[:4]
	for _, w := range static {
		out = append(out, ethcommon.LeftPadBytes(w, 32)...)
	}
	out = append(out, ethcommon.LeftPadBytes(big.NewInt(int64(32*(len(static)+1))).Bytes(), 32)...)
	out = append(out, ethcommon.LeftPadBytes(big.NewInt(int64(len(s))).Bytes(), 32)...)
	padded := make([]byte, (len(s)+31)/32*32)
	copy(padded, s)
	return append(out, padded...)
}

// sendValue sends value with data from key and returns the receipt.
func sendValue(ctx context.Context, a *Anvil, key *ecdsa.PrivateKey, to ethcommon.Address, value *big.Int, data []byte, gas uint64) (*ethtypes.Receipt, error) {
	nonce, err := a.cli.PendingNonceAt(ctx, cryptoPubkeyAddr(key))
	if err != nil {
		return nil, err
	}
	tx, err := ethtypes.SignTx(ethtypes.NewTx(&ethtypes.DynamicFeeTx{ChainID: a.ChainID, Nonce: nonce, GasTipCap: big.NewInt(1e9),
		GasFeeCap: big.NewInt(20e9), Gas: gas, To: &to, Value: value, Data: data}), ethtypes.LatestSignerForChainID(a.ChainID), key)
	if err != nil {
		return nil, err
	}
	if err := a.cli.SendTransaction(ctx, tx); err != nil {
		return nil, err
	}
	return a.waitReceipt(ctx, tx.Hash(), 30*time.Second)
}

func TestEVMBridge_W8_LaunchFixes(t *testing.T) {
	if testing.Short() {
		t.Skip("devnet")
	}
	t.Parallel()
	ctx, cancel := devnetWithTimeout(context.Background(), 140*time.Minute)
	t.Cleanup(cancel)

	e := setupEvmBridgeDevnet(t, ctx)
	anvil := StartAnvil(t, ctx, evmChainID, nextAnvilPort())
	t.Cleanup(anvil.Stop)
	eth := func(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(1e18)) }
	usd := func(n int64) *big.Int { return big.NewInt(n * 1_000_000) }

	// RP-8: the bot is NOT registered as a relayer.
	botKey, _ := ethCrypto.GenerateKey()
	botDID := dids.NewEthDID(ethCrypto.PubkeyToAddress(botKey.PublicKey).Hex()).String()
	if err := e.fundDID(botDID, "20000.000"); err != nil {
		t.Fatalf("fund bot DID for RC: %v", err)
	}
	ownerKey := mustKey(t)
	if err := anvil.SetBalance(ctx, cryptoPubkeyAddr(ownerKey), eth(10)); err != nil {
		t.Fatal(err)
	}
	usdc, err := anvil.DeployMockUSDC(ctx, ownerKey)
	if err != nil {
		t.Fatalf("deploy MockUSDC: %v", err)
	}
	if err := e.proposeExecute("registerToken", fmt.Sprintf(`{"address":"%s","symbol":"usdc","decimals":6,"min_withdrawal":1000000}`, usdc.Hex())); err != nil {
		t.Fatalf("registerToken usdc: %v", err)
	}

	t.Logf("waiting for TSS primary keygen...")
	e.botAttests = true
	vault, err := e.tssVault(25 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := anvil.SetBalance(ctx, vault, eth(10)); err != nil { // L1 gas for the withdrawals
		t.Fatalf("fund vault gas: %v", err)
	}
	bot := StartBot(t, ctx, BotConfig{
		EthRPC: anvil.URL, ContractID: e.amID, Tokens: usdc.Hex(),
		GraphQLURL: e.d.GQLEndpoint(e.gqlNode), EthPrivHex: hex.EncodeToString(ethCrypto.FromECDSA(botKey)),
	})
	t.Cleanup(bot.Stop)
	botLogs := func() string { return strings.Join(bot.Logs(), "\n") }
	if !bot.WaitForLog(ctx, "E23: attested the vault key's check signature", evmL2Wait+10*time.Minute) {
		t.Fatalf("the bot never attested the vault key's check signature:\n%s", botLogs())
	}
	if !bot.WaitForLog(ctx, "A18: vault is the TSS key's address", evmL2Wait) {
		t.Fatalf("the bot did not take the vault:\n%s", botLogs())
	}
	t.Logf("(0) PASS: vault %s live; bot %s is not a registered relayer", vault.Hex(), botDID)

	// (1) RP-8: a plain ETH deposit, mapped by the unregistered bot.
	depKey := mustKey(t)
	depAddr := cryptoPubkeyAddr(depKey)
	if err := anvil.SetBalance(ctx, depAddr, eth(7)); err != nil {
		t.Fatal(err)
	}
	_, depBlock, err := anvil.SendETH(ctx, depKey, vault, eth(6))
	if err != nil {
		t.Fatalf("deposit: %v", err)
	}
	if err := e.plantFinalized(anvil, depBlock); err != nil {
		t.Fatal(err)
	}
	user := evmDID(depAddr)
	if err := pollUntil(ctx, 8*time.Minute, func() bool { return e.stateStr("a-"+user+"-eth") == "5940000000000000000" }); err != nil {
		t.Fatalf("RP-8: the unregistered bot never mapped the ETH deposit: %q\nbot:\n%s", e.stateStr("a-"+user+"-eth"), botLogs())
	}
	t.Logf("(1) PASS RP-8: an unregistered bot mapped the ETH deposit (%s credited 5.94 ETH)", user)

	// (2) RP-7: MagiDeposit for this vault, deployed by CREATE2.
	creationHex, err := os.ReadFile(evmArtifact(t, "EVM_MAGI_DEPOSIT_HEX"))
	if err != nil {
		t.Fatal(err)
	}
	initCode := append(ethcommon.FromHex(strings.TrimSpace(string(creationHex))), ethcommon.LeftPadBytes(vault.Bytes(), 32)...)
	if rc, err := anvil.SendTx(ctx, ownerKey, &create2Deployer, append(make([]byte, 32), initCode...), 1_000_000); err != nil || rc.Status != 1 {
		t.Fatalf("deploy MagiDeposit through the CREATE2 proxy: %v %+v", err, rc)
	}
	dc := ethCrypto.CreateAddress2(create2Deployer, [32]byte{}, ethCrypto.Keccak256(initCode))
	if code, err := anvil.cli.CodeAt(ctx, dc, nil); err != nil || len(code) == 0 {
		t.Fatalf("no MagiDeposit code at %s: %v", dc.Hex(), err)
	}
	if !bot.WaitForLog(ctx, strings.ToLower(dc.Hex()), time.Minute) {
		t.Fatalf("the bot derives another MagiDeposit address than %s:\n%s", dc.Hex(), botLogs())
	}
	t.Logf("(2a) PASS: MagiDeposit at %s (bot and CREATE2 agree)", dc.Hex())

	walletKey := mustKey(t) // stands for a smart wallet / custodian: its own address is not the recipient
	wallet := cryptoPubkeyAddr(walletKey)
	recipKey := mustKey(t)
	recipient := evmDID(cryptoPubkeyAddr(recipKey))
	if err := anvil.SetBalance(ctx, wallet, eth(5)); err != nil {
		t.Fatal(err)
	}
	rc, err := sendValue(ctx, anvil, walletKey, dc, eth(2), depositCalldata("deposit(string)", nil, recipient), 200_000)
	if err != nil || rc.Status != 1 {
		t.Fatalf("MagiDeposit.deposit: %v %+v", err, rc)
	}
	if err := e.plantFinalized(anvil, rc.BlockNumber.Uint64()); err != nil {
		t.Fatal(err)
	}
	if err := pollUntil(ctx, 8*time.Minute, func() bool { return e.stateStr("a-"+recipient+"-eth") == "1980000000000000000" }); err != nil {
		t.Fatalf("RP-7: the ETH MagiDeposit deposit was not credited to %s: %q\nbot:\n%s", recipient, e.stateStr("a-"+recipient+"-eth"), botLogs())
	}
	if got := e.stateStr("a-" + evmDID(wallet) + "-eth"); got != "" {
		t.Fatalf("RP-7: the sending wallet was credited %s", got)
	}
	t.Logf("(2b) PASS RP-7 ETH: 2 ETH through MagiDeposit credited %s 1.98 ETH, the wallet nothing", recipient)

	if rc, err := anvil.SendTx(ctx, ownerKey, &usdc, usdcCall("mint(address,uint256)", wallet, usd(10)), 100_000); err != nil || rc.Status != 1 {
		t.Fatalf("mint: %v", err)
	}
	if rc, err := anvil.SendTx(ctx, walletKey, &usdc, usdcCall("approve(address,uint256)", dc, usd(7)), 100_000); err != nil || rc.Status != 1 {
		t.Fatalf("approve: %v", err)
	}
	rc, err = anvil.SendTx(ctx, walletKey, &dc, depositCalldata("depositToken(address,uint256,string)",
		[][]byte{usdc.Bytes(), usd(7).Bytes()}, recipient), 300_000)
	if err != nil || rc.Status != 1 {
		t.Fatalf("MagiDeposit.depositToken: %v %+v", err, rc)
	}
	if err := e.plantFinalized(anvil, rc.BlockNumber.Uint64()); err != nil {
		t.Fatal(err)
	}
	if err := pollUntil(ctx, 8*time.Minute, func() bool { return e.stateStr("a-"+recipient+"-usdc") == "7000000" }); err != nil {
		t.Fatalf("RP-7: the USDC MagiDeposit deposit was not credited to %s: %q\nbot:\n%s", recipient, e.stateStr("a-"+recipient+"-usdc"), botLogs())
	}
	devnetSleep(30 * time.Second) // a second (wrong) map would have landed by now
	if got := e.stateStr("a-" + evmDID(wallet) + "-usdc"); got != "" {
		t.Fatalf("RP-7: the paying Transfer was also credited to the wallet: %s", got)
	}
	for _, l := range bot.Logs() {
		if strings.Contains(l, "dead") && strings.Contains(strings.ToLower(l), strings.ToLower(rc.TxHash.Hex())) {
			t.Fatalf("RP-7: the bot dead-lettered part of the MagiDeposit tx: %s", l)
		}
	}
	t.Logf("(2c) PASS RP-7 USDC: 7 USDC through MagiDeposit credited %s; the paying Transfer not credited to the wallet, nothing dead-lettered", recipient)

	// (3) RP-11 + GLAM-1: two users withdraw at once.
	if err := e.fundDID(user, "10000.000"); err != nil {
		t.Fatal(err)
	}
	if err := e.fundDID(recipient, "10000.000"); err != nil {
		t.Fatal(err)
	}
	uK, _ := NewEthKey(hex.EncodeToString(ethCrypto.FromECDSA(depKey)))
	rK, _ := NewEthKey(hex.EncodeToString(ethCrypto.FromECDSA(recipKey)))
	to1, to2 := cryptoPubkeyAddr(mustKey(t)), cryptoPubkeyAddr(mustKey(t))
	if _, err := e.d.CallContractEvm(ctx, e.gqlNode, uK, e.amID, "unmapETH", fmt.Sprintf(`{"to":"%s","amount":"1000000000000000000","asset":"eth"}`, to1.Hex()), 5000000); err != nil {
		t.Fatalf("unmapETH user: %v", err)
	}
	if _, err := e.d.CallContractEvm(ctx, e.gqlNode, rK, e.amID, "unmapETH", fmt.Sprintf(`{"to":"%s","amount":"500000000000000000","asset":"eth","gas_limit":100000}`, to2.Hex()), 5000000); err != nil {
		t.Fatalf("unmapETH recipient: %v", err)
	}
	if err := pollUntil(ctx, evmL2Wait, func() bool { return e.stateStr("np") == "2" && e.stateStr("d-1") != "" }); err != nil {
		t.Fatalf("RP-11: the second withdrawal was not issued while the first is in flight: np=%q d-1=%q", e.stateStr("np"), e.stateStr("d-1"))
	}
	t.Logf("(3a) PASS RP-11: two withdrawals in flight (nonces 0 and 1)")
	mined := map[uint64]*ethtypes.Transaction{}
	blocks := map[uint64]uint64{}
	if err := pollUntil(ctx, 25*time.Minute, func() bool {
		for n := uint64(0); n < 2; n++ {
			if mined[n] != nil {
				continue
			}
			if tx, b, found, ferr := anvil.FindTxFrom(ctx, vault, n, depBlock); ferr == nil && found {
				mined[n], blocks[n] = tx, b
			}
		}
		return len(mined) == 2
	}); err != nil {
		t.Fatalf("the vault did not send both withdrawals (got %d)\nbot:\n%s", len(mined), botLogs())
	}
	byTo := map[ethcommon.Address]*ethtypes.Transaction{}
	for _, tx := range mined {
		byTo[*tx.To()] = tx
	}
	if byTo[to2] == nil || byTo[to2].Gas() != 100_000 || byTo[to1] == nil || byTo[to1].Gas() != 21_000 {
		t.Fatalf("GLAM-1: withdrawal gas: to1 %v to2 %v; want 21000 and the requested 100000", byTo[to1], byTo[to2])
	}
	t.Logf("(3b) PASS GLAM-1: the gas_limit withdrawal carried 100000 gas, the default one 21000")
	for _, b := range blocks {
		if err := e.plantFinalized(anvil, b); err != nil {
			t.Fatal(err)
		}
	}
	if err := pollUntil(ctx, 8*time.Minute, func() bool { return e.stateStr("n") == "2" }); err != nil {
		t.Fatalf("RP-11: the bot did not confirm both withdrawals: n=%q\nbot:\n%s", e.stateStr("n"), botLogs())
	}
	t.Logf("(3c) PASS RP-11: the bot confirmed both in nonce order (n=2)")
}
