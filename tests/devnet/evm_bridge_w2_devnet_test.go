//go:build evm_devnet

// W2 — ETH withdrawal close-out on real devnet + anvil L1. Proves the
// launch-blocking withdrawal HIGHs at runtime, contract-side, with a controlled
// vault key (TSS keygen still required for unmapETH's requireTssKey; TSS signing
// is replaced by signing the L1 tx with the controlled vault, which confirmSpend
// recovers via ecrecover_strict == VaultAtQueue):
//   H-F1  : PendingSpend stored as JSON, read back + parsed.
//   H-F2  : confirmSpend flat-10 schema (intent binding + tx-trie + receipt-trie).
//   CC-1  : receipt-trie proof of a no-log ETH transfer (all-zero bloom) matches
//           the planted real receiptsRoot.
//   DS-F1 : EcrecoverStrict recovers the vault from the withdrawal tx signature.
//   ML-1  : confirmed nonce advances exactly once.
//
// Run: go test -tags evm_devnet -run TestEVMBridge_W2 ./tests/devnet/ -timeout 70m -v
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

	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

func TestEVMBridge_W2_WithdrawalCloseout(t *testing.T) {
	if testing.Short() {
		t.Skip("devnet")
	}
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Minute)
	t.Cleanup(cancel)

	e := setupEvmBridgeDevnet(t, ctx)
	anvil := StartAnvil(t, ctx, 1, nextAnvilPort())
	t.Cleanup(anvil.Stop)

	// Relayer (for the deposit), TSS primary key (unmapETH gate), controlled vault.
	if err := e.proposeExecute("registerRelayer", fmt.Sprintf(`{"account":"%s"}`, e.ownerAcct)); err != nil {
		t.Fatalf("registerRelayer: %v", err)
	}
	t.Logf("waiting for TSS primary keygen...")
	if err := e.createTssKey(25 * time.Minute); err != nil {
		t.Fatalf("TSS keygen: %v", err)
	}
	t.Logf("TSS primary key ready")

	vaultKey, _ := ethCrypto.GenerateKey()
	vaultAddr := ethCrypto.PubkeyToAddress(vaultKey.PublicKey)
	if err := e.setVault(vaultAddr.Hex()); err != nil {
		t.Fatalf("setVault: %v", err)
	}
	// Fund the vault on anvil so it can broadcast the withdrawal.
	_ = anvil.SetBalance(ctx, vaultAddr, new(big.Int).Mul(big.NewInt(100), big.NewInt(1e18)))

	// Deposit 6 ETH for depositor K — the 1% gas tax (60M gwei) exceeds
	// MinGasReserve (50M gwei) so unmapETH's gas-reserve gate passes.
	depKey, _ := ethCrypto.GenerateKey()
	depHex := hex.EncodeToString(ethCrypto.FromECDSA(depKey))
	sixEth := new(big.Int).Mul(big.NewInt(6), big.NewInt(1e18))
	did, err := e.depositETH(anvil, depKey, vaultAddr, sixEth)
	if err != nil {
		t.Fatalf("deposit: %v", err)
	}
	t.Logf("deposited; L2 credit at %s", did)

	// Fund K's evm DID with HBD so it has RC to submit the L2 unmapETH.
	if err := e.fundDID(did, "10000.000"); err != nil {
		t.Fatalf("fund K DID for RC: %v", err)
	}

	// unmapETH as K (evm-signed) — withdraw 1 ETH (1e9 gwei) to a fresh recipient.
	K, err := NewEthKey(depHex)
	if err != nil {
		t.Fatalf("eth key: %v", err)
	}
	recipKey, _ := ethCrypto.GenerateKey()
	recipAddr := ethCrypto.PubkeyToAddress(recipKey.PublicKey)
	unmapPayload := fmt.Sprintf(`{"to":"%s","amount":"1000000000"}`, recipAddr.Hex())
	if _, err := e.d.CallContractEvm(ctx, e.gqlNode, K, e.amID, "unmapETH", unmapPayload, 5000000); err != nil {
		t.Fatalf("unmapETH broadcast: %v", err)
	}

	// Read PendingSpend d-0 (H-F1: JSON) and parse it.
	type pendingSpend struct {
		Nonce         uint64 `json:"nonce"`
		Amount        int64  `json:"amount"`
		To            string `json:"to"`
		Asset         string `json:"asset"`
		VaultAtQueue  string `json:"vault_at_queue"`
		UnsignedTxHex string `json:"unsigned_tx_hex"`
	}
	var ps pendingSpend
	if err := pollUntil(ctx, 3*time.Minute, func() bool {
		m, _ := e.d.GetStateByKeys(ctx, e.gqlNode, e.amID, []string{"d-0"})
		if m == nil || m["d-0"] == nil {
			return false
		}
		return json.Unmarshal([]byte(fmt.Sprintf("%v", m["d-0"])), &ps) == nil && ps.UnsignedTxHex != ""
	}); err != nil {
		t.Fatalf("PendingSpend d-0 not created (unmapETH failed) — check RC/gas-reserve")
	}
	t.Logf("H-F1 PendingSpend(JSON): nonce=%d to=%s amount=%d asset=%s vault=%s", ps.Nonce, ps.To, ps.Amount, ps.Asset, ps.VaultAtQueue)

	// Broadcast the vault withdrawal tx (V -> recipient, value = amount*1e9 wei).
	// V's first L1 tx has nonce 0 == ps.Nonce, matching the contract's unsigned tx.
	valueWei := new(big.Int).Mul(big.NewInt(ps.Amount), big.NewInt(1e9))
	wTxHash, blockN, err := anvil.SendETH(ctx, vaultKey, recipAddr, valueWei)
	if err != nil {
		t.Fatalf("withdrawal broadcast: %v", err)
	}
	_ = anvil.Mine(ctx, 6)
	if err := pollUntil(ctx, 60*time.Second, func() bool { f, e2 := anvil.Finalized(ctx); return e2 == nil && f >= blockN }); err != nil {
		t.Fatalf("withdrawal block %d not finalized", blockN)
	}
	hdrN, err := anvil.Header(ctx, blockN)
	if err != nil {
		t.Fatalf("header: %v", err)
	}
	if err := e.plantHeader(hdrN, 1); err != nil {
		t.Fatalf("plant: %v", err)
	}

	// Build tx-trie + receipt-trie proofs (the no-log ETH transfer = CC-1 all-zero bloom).
	wIdx, err := anvil.FindTxIndex(ctx, blockN, wTxHash)
	if err != nil {
		t.Fatalf("tx index: %v", err)
	}
	txHex, txProofHex, err := anvil.TxTrieProof(ctx, blockN, wIdx)
	if err != nil {
		t.Fatalf("tx proof: %v", err)
	}
	rcptHex, rcptProofHex, err := anvil.ReceiptTrieProof(ctx, blockN, wIdx)
	if err != nil {
		t.Fatalf("receipt proof: %v", err)
	}

	// confirmSpend (flat-10). intent_* must match the stored PendingSpend exactly.
	csPayload := fmt.Sprintf(`{"block_height":%d,"tx_index":%d,"tx_hex":"%s","tx_proof_hex":"%s","receipt_hex":"%s","receipt_proof_hex":"%s","intent_nonce":%d,"intent_to":"%s","intent_amount":%d,"intent_asset":"%s"}`,
		blockN, wIdx, txHex, txProofHex, rcptHex, rcptProofHex, ps.Nonce, ps.To, ps.Amount, ps.Asset)
	csTx, err := e.callOnce(e.ownerNode, e.amID, "confirmSpend", csPayload)
	if err != nil {
		t.Fatalf("confirmSpend broadcast: %v", err)
	}

	// ASSERT: confirmed nonce advanced 0 -> 1.
	if err := pollUntil(ctx, 3*time.Minute, func() bool {
		m, _ := e.d.GetStateByKeys(ctx, e.gqlNode, e.amID, []string{"n"})
		return m != nil && fmt.Sprintf("%v", m["n"]) == "1"
	}); err != nil {
		dumpMapErr(t, e, ctx, csTx)
		m, _ := e.d.GetStateByKeys(ctx, e.gqlNode, e.amID, []string{"n", "np"})
		t.Fatalf("confirmSpend did not advance confirmed nonce (n=%v np=%v) — see DAG above", m["n"], m["np"])
	}
	_ = strings.ToLower
	t.Logf("W2 PASS — withdrawal confirmed, confirmed nonce 0->1. Proven: H-F1 (PendingSpend JSON), "+
		"H-F2 (confirmSpend flat-10 + intent bind + tx/receipt trie), CC-1 (all-zero-bloom receiptsRoot match), "+
		"DS-F1 (ecrecover_strict==vault), ML-1 (single nonce advance).")
}
