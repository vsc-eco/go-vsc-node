//go:build evm_realwasm

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

// EVM-FEE-1 on the bridge wasm: an ETH withdrawal signed with a 250,000 gas
// limit at fee cap 40 gwei (prepaid 0.01 ETH) mines as the block's second
// transaction using 21,000 gas at 1 gwei base + 2 gwei tip. confirmSpend
// records the settlement; the bot's refundWithdrawalGas payload (the previous
// receipt's proof) refunds 0.01 - 21,000 x 3 gwei to the withdrawer, once.
func TestRealWasm_EVMFee1_RefundsTheUnusedGas(t *testing.T) {
	ct := rwContract(t)
	vaultKey, _ := ethCrypto.GenerateKey()
	vault := ethCrypto.PubkeyToAddress(vaultKey.PublicKey)
	otherKey, _ := ethCrypto.GenerateKey()
	to := common.HexToAddress("0x00000000000000000000000000000000000000e2")
	c := &fakeChain{}
	c.addTx(t, otherKey, &types.DynamicFeeTx{Nonce: 7, GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(10e9), Gas: 21000, To: &to, Value: big.NewInt(1)})
	c.addTx(t, vaultKey, &types.DynamicFeeTx{Nonce: 0, GasTipCap: big.NewInt(2e9), GasFeeCap: big.NewInt(40e9), Gas: 250_000, To: &to, Value: big.NewInt(1e18)})
	rwHeader(ct, c)
	prepaid := new(big.Int).Mul(big.NewInt(250_000), big.NewInt(40e9))
	raw, _ := json.Marshal(map[string]any{
		"nonce": 0, "amount": big.NewInt(1e18), "from": "hive:alice", "to": to.Hex(), "asset": "eth",
		"unsigned_tx_hex": "02ee", "block_height": 100, "vault_at_queue": vault.Hex(),
		"gas_cost": prepaid, "gas_limit": 250_000,
	})
	ct.StateSet(rwBridge, "d-0", string(raw))
	ct.StateSet(rwBridge, "np", "1")
	ps := parsePendingSpend(0, string(raw))
	if ps == nil {
		t.Fatal("bot refused the stored PendingSpend")
	}
	payload, err := buildConfirmSpendPayload(context.Background(), c.rpc(t), c.txs[1].Hash().Hex(), 101, 1, ps)
	if err != nil {
		t.Fatal(err)
	}
	r := rwCall(ct, rwRelayer, "confirmSpend", payload)
	if !r.Success {
		t.Fatalf("confirmSpend: %s: %s", r.Err, r.ErrMsg)
	}
	t.Logf("confirmSpend RC %d (payload %d bytes)", r.RcUsed, len(payload))
	if ct.StateGet(rwBridge, "gsl") != "0" || ct.StateGet(rwBridge, "gs-0") == "" {
		t.Fatalf("settlement not recorded: gsl %q", ct.StateGet(rwBridge, "gsl"))
	}
	if b := ct.StateGet(rwBridge, "a-hive:alice-eth"); b != "" && b != "0" {
		t.Fatalf("refunded before the settlement: %q", b)
	}

	// The bot's refund payload: the receipt before the withdrawal in its block.
	bp, err := fetchBlockProofs(context.Background(), c.rpc(t), 101, false)
	if err != nil {
		t.Fatal(err)
	}
	_, nodes, err := bp.receiptProof(0)
	if err != nil {
		t.Fatal(err)
	}
	rp, _ := json.Marshal(map[string]any{"nonce": 0, "receipt_proof": base64.StdEncoding.EncodeToString(bytes.Join(nodes, nil))})
	r = rwCall(ct, rwRelayer, "refundWithdrawalGas", rp)
	if !r.Success {
		t.Fatalf("refundWithdrawalGas: %s: %s", r.Err, r.ErrMsg)
	}
	t.Logf("refundWithdrawalGas RC %d (payload %d bytes)", r.RcUsed, len(rp))
	want := new(big.Int).Sub(prepaid, big.NewInt(21_000*3e9))
	if got := ct.StateGet(rwBridge, "a-hive:alice-eth"); got != want.String() {
		t.Fatalf("withdrawer refund %q, want %s", got, want)
	}
	if ct.StateGet(rwBridge, "gs-0") != "" || ct.StateGet(rwBridge, "gsl") != "" {
		t.Fatal("settlement record not cleared")
	}
	if r = rwCall(ct, rwRelayer, "refundWithdrawalGas", rp); r.Success {
		t.Fatal("a second refund must be refused")
	}
}
