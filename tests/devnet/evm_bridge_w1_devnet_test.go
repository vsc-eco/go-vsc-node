//go:build evm_devnet

// W1 — ETH deposit pipeline on real devnet + anvil L1. Proves the deposit-side
// fixes at runtime: ecrecover_canonical (recovers the L1 depositor from the
// EIP-1559 tx), tx-trie proof verification against a REAL anvil block root
// (planted via the mock verifier), and the credit landing at the depositor's
// derived DID. No TSS needed (deposits aren't TSS-gated).
//
// Run: go test -tags evm_devnet -run TestEVMBridge_W1 ./tests/devnet/ -timeout 50m -v
package devnet

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

func TestEVMBridge_W1_DepositETH(t *testing.T) {
	if testing.Short() {
		t.Skip("devnet")
	}
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Minute)
	t.Cleanup(cancel)

	e := setupEvmBridgeDevnet(t, ctx)

	anvil := StartAnvil(t, ctx, 1, nextAnvilPort()) // contract chainId() defaults to 1
	t.Cleanup(anvil.Stop)

	// Vault (deposits land here on L1) + register the relayer (ETH map is
	// relayer-gated) + setVault. ownerAcct (magi.test1) is the relayer.
	vaultKey, _ := ethCrypto.GenerateKey()
	vaultAddr := ethCrypto.PubkeyToAddress(vaultKey.PublicKey)
	if err := e.proposeExecute("registerRelayer", fmt.Sprintf(`{"account":"%s"}`, e.ownerAcct)); err != nil {
		t.Fatalf("registerRelayer: %v", err)
	}
	if err := e.setVault(vaultAddr.Hex()); err != nil {
		t.Fatalf("setVault: %v", err)
	}
	t.Logf("vault=%s relayer=%s", vaultAddr.Hex(), e.ownerAcct)

	// Depositor K: fund on anvil, send 1 ETH to the vault.
	depKey, _ := ethCrypto.GenerateKey()
	depAddr := ethCrypto.PubkeyToAddress(depKey.PublicKey)
	tenEth := new(big.Int).Mul(big.NewInt(10), big.NewInt(1e18))
	if err := anvil.SetBalance(ctx, depAddr, tenEth); err != nil {
		t.Fatalf("fund depositor: %v", err)
	}
	oneEth := big.NewInt(1e18)
	txHash, blockM, err := anvil.SendETH(ctx, depKey, vaultAddr, oneEth)
	if err != nil {
		t.Fatalf("deposit tx: %v", err)
	}
	t.Logf("deposit tx %s in anvil block %d (depositor %s)", txHash.Hex(), blockM, depAddr.Hex())

	// Advance anvil so block M is finalized, then plant its real header.
	_ = anvil.Mine(ctx, 6)
	if err := pollUntil(ctx, 60*time.Second, func() bool {
		f, e2 := anvil.Finalized(ctx)
		return e2 == nil && f >= blockM
	}); err != nil {
		t.Fatalf("anvil block %d never finalized", blockM)
	}
	hdr, err := anvil.Header(ctx, blockM)
	if err != nil {
		t.Fatalf("read header: %v", err)
	}
	if err := e.plantHeader(hdr, 1); err != nil {
		t.Fatalf("plant header: %v", err)
	}
	t.Logf("planted header %d (txRoot %s)", blockM, hdr.TxRoot)

	// Build the deposit proof (tx-trie, root-verified locally) + relay via map.
	txIndex, err := anvil.FindTxIndex(ctx, blockM, txHash)
	if err != nil {
		t.Fatalf("tx index: %v", err)
	}
	rawHex, proofHex, err := anvil.TxTrieProof(ctx, blockM, txIndex)
	if err != nil {
		t.Fatalf("build proof: %v", err)
	}
	payload := fmt.Sprintf(`{"tx_data":{"block_height":%d,"tx_index":%d,"raw_hex":"%s","merkle_proof_hex":"%s","deposit_type":"eth"},"instructions":[]}`,
		blockM, txIndex, rawHex, proofHex)
	// Single-broadcast: `map` is non-idempotent (no L2 nonce dedup); CallContract's
	// 3x retry can double-process -> "deposit already processed" with net-zero state.
	mapTx, err := e.callOnce(e.ownerNode, e.amID, "map", payload)
	if err != nil {
		t.Fatalf("map broadcast: %v", err)
	}

	// ASSERT: depositor's derived DID is credited the ETH (in gwei).
	did := "did:pkh:eip155:1:" + strings.ToLower(depAddr.Hex())
	balKey := "a-" + did + "-eth"
	if err := pollUntil(ctx, 3*time.Minute, func() bool {
		m, _ := e.d.GetStateByKeys(ctx, e.gqlNode, e.amID, []string{balKey})
		if m == nil || m[balKey] == nil {
			return false
		}
		return fmt.Sprintf("%v", m[balKey]) != "" && fmt.Sprintf("%v", m[balKey]) != "0"
	}); err != nil {
		// Diagnostic: dump the map call's contract output (the real abort if any).
		dumpMapErr(t, e, ctx, mapTx)
		// State diagnostics: did credit happen anywhere (supply), is the deposit
		// observed, and what's at the expected balance key?
		// Compute the EXACT observedEntryKey the contract checks (o-{h}-hex(txHash||index)).
		rawB, _ := hex.DecodeString(rawHex)
		txh := ethCrypto.Keccak256(rawB) // == signed.Hash() for typed tx
		entry := append(append([]byte{}, txh...), byte(txIndex>>8), byte(txIndex))
		obKey := fmt.Sprintf("o-%d-%s", blockM, hex.EncodeToString(entry))
		// Sanity keys (KNOWN set) to validate that our state reads work + node is current.
		diag, _ := e.d.GetStateByKeys(ctx, e.gqlNode, e.amID, []string{
			"vault", "rl-" + e.ownerAcct, "s-eth", fmt.Sprintf("oc-%d", blockM), balKey, obKey,
		})
		t.Logf("DIAG sanity vault=%v relayer(rl-%s)=%v", diag["vault"], e.ownerAcct, diag["rl-"+e.ownerAcct])
		t.Logf("DIAG supply(s-eth)=%v observedCount(oc-%d)=%v balance(%s)=%v observedEntry(%s)=%v",
			diag["s-eth"], blockM, diag[fmt.Sprintf("oc-%d", blockM)], balKey, diag[balKey], obKey, diag[obKey])
		t.Logf("DIAG depositor=%s vault=%s blockM=%d txIndex=%d", depAddr.Hex(), vaultAddr.Hex(), blockM, txIndex)
		m, _ := e.d.GetStateByKeys(ctx, e.gqlNode, e.amID, []string{balKey})
		t.Fatalf("deposit not credited to %s (bal=%v) — see DIAG/DAG above", did, m[balKey])
	}
	m, _ := e.d.GetStateByKeys(ctx, e.gqlNode, e.amID, []string{balKey})
	t.Logf("W1 PASS — ETH deposit credited %s = %v gwei (ecrecover_canonical recovered depositor, tx-trie verified vs real anvil root)", did, m[balKey])
	_ = ethcommon.Address{}
}

// dumpMapErr reads the map tx's contract-output ret (the hidden abort message).
func dumpMapErr(t *testing.T, e *evmBridgeEnv, ctx context.Context, txID string) {
	const q = `query($id:String!){findTransaction(filterOptions:{byId:$id}){id status output{id}}}`
	var out struct {
		FindTransaction []struct {
			Id, Status string
			Output     []struct {
				Id string `json:"id"`
			} `json:"output"`
		} `json:"findTransaction"`
	}
	if err := e.d.gqlQuery(ctx, e.gqlNode, q, map[string]any{"id": txID}, &out); err != nil {
		t.Logf("map DAG query err: %v", err)
		return
	}
	for _, tx := range out.FindTransaction {
		t.Logf("map tx %s status=%s", tx.Id, tx.Status)
		for _, o := range tx.Output {
			const dq = `query($c:String!){getDagByCID(cidString:$c)}`
			var d2 struct {
				GetDagByCID any `json:"getDagByCID"`
			}
			if err := e.d.gqlQuery(ctx, e.gqlNode, dq, map[string]any{"c": o.Id}, &d2); err == nil {
				t.Logf("map output %s = %v", o.Id, d2.GetDagByCID)
			}
		}
	}
}
