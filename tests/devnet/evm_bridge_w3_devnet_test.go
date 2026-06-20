//go:build evm_devnet

// W3 — Double-spend attack on a SUCCEEDED ETH withdrawal (real devnet + anvil L1).
// Proves the launch-blocking state-machine HIGHs at runtime, contract-side:
//   H-SM2 (Type-B disabled): a `block_inclusion_without_tx` drop-proof is
//         REJECTED. Its premise ("vault advanced past nonce N ⇒ tx N dropped")
//         is unsound — a SUCCEEDED withdrawal also advances the nonce — so the
//         contract disables Type-B entirely (handlers.go:1688). cancelMyWithdrawal
//         with a Type-B proof MUST abort.
//   H-SM1 (Type-A needs status==0): a `reverted_receipt` drop-proof carrying the
//         SUCCEEDED withdrawal's real receipt (status==1) is REJECTED with
//         "receipt status != 0 — tx not reverted" (handlers.go:1559). A tx that
//         paid the recipient on L1 cannot be claimed as reverted.
//   No double-spend: after BOTH failed attacks the PendingSpend d-0 still exists,
//         the confirmed nonce `n` did NOT advance, and the depositor's eth balance
//         was NOT re-credited above its post-unmap value. The withdrawal can only
//         be closed by confirmSpend (W2), never force-refunded.
//
// Mirrors W2 up to the withdrawal SUCCEEDING on L1, then attacks instead of
// calling confirmSpend.
//
// Run: go test -tags evm_devnet -run TestEVMBridge_W3 ./tests/devnet/ -timeout 70m -v
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

func TestEVMBridge_W3_DoubleSpendAttack(t *testing.T) {
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

	// Fund K's evm DID with HBD so it has RC for THREE L2 calls (unmapETH +
	// the two cancelMyWithdrawal attacks), each gated on available RC >= 5M
	// rc_limit. 30000 HBD (~30M RC) clears all three with margin.
	if err := e.fundDID(did, "30000.000"); err != nil {
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

	// Read PendingSpend d-0 (JSON) and parse it.
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
	t.Logf("PendingSpend(JSON): nonce=%d to=%s amount=%d asset=%s vault=%s", ps.Nonce, ps.To, ps.Amount, ps.Asset, ps.VaultAtQueue)

	// Record the depositor's post-unmap eth balance — it must NOT increase
	// across either attack (no force-refund). unmapETH already debited
	// amount + fee from K, so this is the floor the attacks must not exceed.
	balKey := fmt.Sprintf("a-%s-eth", did)
	postUnmapBal := readIntState(t, e, ctx, balKey)
	t.Logf("post-unmap depositor balance %s = %d gwei", balKey, postUnmapBal)

	// Broadcast the vault withdrawal tx (V -> recipient, value = amount*1e9 wei).
	// V's first L1 tx has nonce 0 == ps.Nonce, matching the contract's unsigned tx.
	// This SUCCEEDS on L1 (status==1, recipient PAID) — the precise condition under
	// which a drop-proof must NEVER be accepted.
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

	// Build tx-trie + receipt-trie proofs for the SUCCEEDED withdrawal.
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
	t.Logf("SUCCEEDED withdrawal mined: block=%d txIndex=%d hash=%s (recipient %s PAID)", blockN, wIdx, wTxHash, recipAddr.Hex())

	// ──────────────────────────────────────────────────────────────────────
	// ATTACK A — H-SM2: Type-B "block_inclusion_without_tx" drop-proof.
	// The attacker (depositor K = ps.From, the only allowed canceller) tries to
	// force-refund the SUCCEEDED withdrawal by claiming a "drop" via the disabled
	// Type-B shape, feeding the SUCCEEDED tx's own tx-trie proof. The contract
	// MUST reject. Two valid rejection gates may fire first (both prove no
	// double-spend): (1) the SUCCEEDED tx is at nonce 0 == ps.Nonce, so the
	// "nonce does not exceed cleared nonce — vault has not advanced" gate trips;
	// (2) the Type-B-disabled H-SM2 gate. We accept either.
	// ──────────────────────────────────────────────────────────────────────
	typeBProof := fmt.Sprintf(`{"type":"block_inclusion_without_tx","block_height":%d,"tx_index":%d,"tx_nonce":%d,"tx_at_index_hex":"%s","tx_proof_hex":"%s"}`,
		blockN, wIdx, ps.Nonce, txHex, txProofHex)
	cancelB := fmt.Sprintf(`{"nonce":%d,"proof":%s}`, ps.Nonce, typeBProof)
	// Call AS K (the original withdrawer / ps.From) via evm-DID signing — the
	// contract's cancelMyWithdrawal authorizes only the original withdrawer, so
	// an owner-signed call (callOnce) is rejected at the auth gate before reaching
	// the Type-B/Type-A guards. K already has RC (funded 10000 HBD above).
	txB, err := e.d.CallContractEvm(ctx, e.gqlNode, K, e.amID, "cancelMyWithdrawal", cancelB, 5000000)
	if err != nil {
		t.Fatalf("ATTACK A cancelMyWithdrawal broadcast: %v", err)
	}
	outB, okB := txOutputMsg(t, e, ctx, txB)
	t.Logf("ATTACK A (Type-B) output: ok=%v msg=%q", okB, outB)
	if okB {
		dumpMapErr(t, e, ctx, txB)
		t.Fatalf("ATTACK A SUCCEEDED — Type-B drop-proof force-refunded a SUCCEEDED withdrawal (H-SM2 BROKEN, double-spend)")
	}
	if !(strings.Contains(outB, "Type-B") || strings.Contains(outB, "block_inclusion_without_tx") ||
		strings.Contains(outB, "vault has not advanced") || strings.Contains(outB, "does not exceed cleared nonce")) {
		dumpMapErr(t, e, ctx, txB)
		t.Fatalf("ATTACK A rejected for the WRONG reason — expected Type-B-disabled (H-SM2) or nonce-not-advanced; got %q", outB)
	}
	t.Logf("ATTACK A REJECTED (H-SM2): Type-B drop-proof cannot force-refund a SUCCEEDED withdrawal")

	// ──────────────────────────────────────────────────────────────────────
	// ATTACK B — H-SM1: Type-A "reverted_receipt" drop-proof carrying the
	// SUCCEEDED withdrawal's REAL receipt (status==1) + tx proof. The receipt
	// proves the tx was mined and PAID; claiming it as "reverted" must fail with
	// "receipt status != 0 — tx not reverted".
	// ──────────────────────────────────────────────────────────────────────
	typeAProof := fmt.Sprintf(`{"type":"reverted_receipt","block_height":%d,"tx_index":%d,"tx_nonce":%d,"receipt_hex":"%s","receipt_proof_hex":"%s","tx_at_index_hex":"%s","tx_proof_hex":"%s"}`,
		blockN, wIdx, ps.Nonce, rcptHex, rcptProofHex, txHex, txProofHex)
	cancelA := fmt.Sprintf(`{"nonce":%d,"proof":%s}`, ps.Nonce, typeAProof)
	// Call AS K (original withdrawer) via evm-DID signing — see ATTACK A note.
	txA, err := e.d.CallContractEvm(ctx, e.gqlNode, K, e.amID, "cancelMyWithdrawal", cancelA, 5000000)
	if err != nil {
		t.Fatalf("ATTACK B cancelMyWithdrawal broadcast: %v", err)
	}
	outA, okA := txOutputMsg(t, e, ctx, txA)
	t.Logf("ATTACK B (Type-A, status==1) output: ok=%v msg=%q", okA, outA)
	if okA {
		dumpMapErr(t, e, ctx, txA)
		t.Fatalf("ATTACK B SUCCEEDED — Type-A drop-proof accepted a SUCCEEDED (status==1) receipt as reverted (H-SM1 BROKEN, double-spend)")
	}
	if !strings.Contains(outA, "receipt status != 0") {
		dumpMapErr(t, e, ctx, txA)
		t.Fatalf("ATTACK B rejected for the WRONG reason — expected \"receipt status != 0\" (H-SM1); got %q", outA)
	}
	t.Logf("ATTACK B REJECTED (H-SM1): Type-A drop-proof of a status==1 receipt rejected (\"receipt status != 0\")")

	// ──────────────────────────────────────────────────────────────────────
	// ASSERT NO DOUBLE-SPEND — the withdrawal state machine is intact:
	//   1. PendingSpend d-0 still exists (not deleted by either attack).
	//   2. Confirmed nonce `n` did NOT advance (absent or 0).
	//   3. Depositor eth balance was NOT re-credited above its post-unmap value.
	// ──────────────────────────────────────────────────────────────────────
	dm, _ := e.d.GetStateByKeys(ctx, e.gqlNode, e.amID, []string{"d-0"})
	if dm == nil || dm["d-0"] == nil {
		t.Fatalf("DOUBLE-SPEND: PendingSpend d-0 was DELETED by a failed attack — withdrawal state corrupted")
	}
	var psAfter pendingSpend
	if err := json.Unmarshal([]byte(fmt.Sprintf("%v", dm["d-0"])), &psAfter); err != nil || psAfter.UnsignedTxHex == "" {
		t.Fatalf("DOUBLE-SPEND: PendingSpend d-0 unreadable/cleared after attacks: %v", dm["d-0"])
	}

	nm, _ := e.d.GetStateByKeys(ctx, e.gqlNode, e.amID, []string{"n"})
	nStr := "0"
	if nm != nil && nm["n"] != nil {
		nStr = strings.TrimSpace(fmt.Sprintf("%v", nm["n"]))
	}
	if nStr != "0" && nStr != "<nil>" && nStr != "" {
		t.Fatalf("DOUBLE-SPEND: confirmed nonce advanced to %q after a failed attack — withdrawal counted as closed without confirmSpend", nStr)
	}

	finalBal := readIntState(t, e, ctx, balKey)
	if finalBal > postUnmapBal {
		t.Fatalf("DOUBLE-SPEND: depositor eth balance re-credited %d -> %d gwei by a failed attack (refund of an already-paid withdrawal)", postUnmapBal, finalBal)
	}

	t.Logf("W3 PASS — succeeded withdrawal cannot be force-refunded (Type-B disabled H-SM2; Type-A needs status==0 H-SM1); no double-spend "+
		"(d-0 present, confirmed nonce n=%q unchanged, depositor balance %d gwei unchanged)", nStr, finalBal)
}

// txOutputMsg reads a map tx's contract-output result: returns (message, ok).
// ok==true means the contract call SUCCEEDED (results[].ok true / no err); a
// non-empty message is the abort/error string when ok==false. Mirrors the DAG
// walk in dumpMapErr (findTransaction{output{id}} -> getDagByCID).
func txOutputMsg(t *testing.T, e *evmBridgeEnv, ctx context.Context, txID string) (string, bool) {
	t.Helper()
	// Poll until the tx is processed and an output DAG is available.
	const q = `query($id:String!){findTransaction(filterOptions:{byId:$id}){id status output{id}}}`
	var outputCID string
	_ = pollUntil(ctx, 3*time.Minute, func() bool {
		var out struct {
			FindTransaction []struct {
				Id, Status string
				Output     []struct {
					Id string `json:"id"`
				} `json:"output"`
			} `json:"findTransaction"`
		}
		if err := e.d.gqlQuery(ctx, e.gqlNode, q, map[string]any{"id": txID}, &out); err != nil {
			return false
		}
		for _, tx := range out.FindTransaction {
			for _, o := range tx.Output {
				if o.Id != "" {
					outputCID = o.Id
					return true
				}
			}
		}
		return false
	})
	if outputCID == "" {
		t.Logf("txOutputMsg: no output DAG for tx %s", txID)
		return "", false
	}
	const dq = `query($c:String!){getDagByCID(cidString:$c)}`
	var d2 struct {
		GetDagByCID any `json:"getDagByCID"`
	}
	if err := e.d.gqlQuery(ctx, e.gqlNode, dq, map[string]any{"c": outputCID}, &d2); err != nil {
		t.Logf("txOutputMsg: getDagByCID err: %v", err)
		return "", false
	}
	// The output DAG is a contract-output object whose results carry ok/err/ret.
	// We stringify the whole thing and look for failure markers + the abort msg;
	// the contract aborts wrap the handler error string (e.g. "receipt status != 0").
	raw := fmt.Sprintf("%v", d2.GetDagByCID)
	blob, _ := json.Marshal(d2.GetDagByCID)
	combined := raw + " " + string(blob)
	// ok if there is no error/abort marker present.
	lc := strings.ToLower(combined)
	ok := !strings.Contains(lc, "err") && !strings.Contains(lc, "abort") && !strings.Contains(lc, "fail")
	return combined, ok
}

// readIntState reads a contract state key and parses it as an int64 (0 if absent).
func readIntState(t *testing.T, e *evmBridgeEnv, ctx context.Context, key string) int64 {
	t.Helper()
	m, _ := e.d.GetStateByKeys(ctx, e.gqlNode, e.amID, []string{key})
	if m == nil || m[key] == nil {
		return 0
	}
	s := strings.TrimSpace(fmt.Sprintf("%v", m[key]))
	s = strings.Trim(s, `"`)
	var v int64
	if _, err := fmt.Sscan(s, &v); err != nil {
		return 0
	}
	return v
}
