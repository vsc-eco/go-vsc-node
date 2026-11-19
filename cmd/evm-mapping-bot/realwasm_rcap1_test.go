//go:build evm_realwasm

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"testing"

	"vsc-node/modules/common/params"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

// CONFIRM-RECEIPT-CAP-1 on the bridge wasm: an ETH withdrawal mined as the
// second transaction of block 101. With a recipient that logs 10 KB its
// confirm is over the node's payload cap; the bot's inclusion-only confirm
// closes it. Reverted, it is settled from its (small) receipt: the value comes
// back and the gas is recorded for refundWithdrawalGas. RC of each call logged.
func TestRealWasm_RCAP1_InclusionCloseAndSettle(t *testing.T) {
	for _, reverted := range []bool{false, true} {
		ct := rwContract(t)
		vaultKey, _ := ethCrypto.GenerateKey()
		vault := ethCrypto.PubkeyToAddress(vaultKey.PublicKey)
		otherKey, _ := ethCrypto.GenerateKey()
		to := common.HexToAddress("0x00000000000000000000000000000000000000e2")
		c := &fakeChain{}
		c.addTx(t, otherKey, &types.DynamicFeeTx{Nonce: 7, GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(10e9), Gas: 21000, To: &to, Value: big.NewInt(1)})
		c.addTx(t, vaultKey, &types.DynamicFeeTx{Nonce: 0, GasTipCap: big.NewInt(2e9), GasFeeCap: big.NewInt(40e9), Gas: 250_000, To: &to, Value: big.NewInt(1e18)},
			&types.Log{Address: to, Data: make([]byte, 10_000)})
		if reverted {
			rc := c.receipts[1]
			rc.Status, rc.Logs = types.ReceiptStatusFailed, nil
			rc.Bloom = types.CreateBloom(types.Receipts{rc})
		}
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
		full, err := buildConfirmSpendPayload(context.Background(), c.rpc(t), c.txs[1].Hash().Hex(), 101, 1, ps)
		if err != nil {
			t.Fatal(err)
		}
		if !reverted && nodePayloadSize(full) <= params.MAX_CONTRACT_PAYLOAD_SIZE {
			t.Fatalf("fixture: the confirm with the receipt is %d bytes, under the cap", nodePayloadSize(full))
		}
		short, err := withoutReceiptProof(full)
		if err != nil {
			t.Fatal(err)
		}
		r := rwCall(ct, rwRelayer, "confirmSpend", short)
		if !r.Success {
			t.Fatalf("reverted=%v inclusion-only confirmSpend: %s: %s", reverted, r.Err, r.ErrMsg)
		}
		t.Logf("reverted=%v: confirm with receipt %d B; inclusion-only confirmSpend RC %d (payload %d B)", reverted, nodePayloadSize(full), r.RcUsed, nodePayloadSize(short))
		if ct.StateGet(rwBridge, "n") != "1" || ct.StateGet(rwBridge, "d-0") != "" || ct.StateGet(rwBridge, "wic-101-1") == "" {
			t.Fatalf("not closed: n %q, record %q", ct.StateGet(rwBridge, "n"), ct.StateGet(rwBridge, "wic-101-1"))
		}

		bp, err := fetchBlockProofs(context.Background(), c.rpc(t), 101, false)
		if err != nil {
			t.Fatal(err)
		}
		_, nodes, err := bp.receiptProof(1)
		if err != nil {
			t.Fatal(err)
		}
		sp, _ := json.Marshal(map[string]any{"block_height": 101, "tx_index": 1, "receipt_proof": base64.StdEncoding.EncodeToString(bytes.Join(nodes, nil))})
		if !reverted {
			if n := nodePayloadSize(sp); n <= params.MAX_CONTRACT_PAYLOAD_SIZE {
				t.Fatalf("fixture: the 10 KB receipt's settlement is %d bytes, under the cap", n)
			}
			continue // unsettleable: stays paid without its gas refund
		}
		r = rwCall(ct, rwRelayer, "settleWithdrawalReceipt", sp)
		if !r.Success {
			t.Fatalf("settleWithdrawalReceipt: %s: %s", r.Err, r.ErrMsg)
		}
		t.Logf("settleWithdrawalReceipt RC %d (payload %d B)", r.RcUsed, nodePayloadSize(sp))
		if got := ct.StateGet(rwBridge, "a-hive:alice-eth"); got != "1000000000000000000" {
			t.Fatalf("revert refund %q, want 1 ETH", got)
		}
		if ct.StateGet(rwBridge, "wic-101-1") != "" || ct.StateGet(rwBridge, "gsl") != "0" {
			t.Fatalf("record %q, gsl %q: want the record gone and the gas recorded", ct.StateGet(rwBridge, "wic-101-1"), ct.StateGet(rwBridge, "gsl"))
		}
		if r = rwCall(ct, rwRelayer, "settleWithdrawalReceipt", sp); r.Success {
			t.Fatal("a second settlement must be refused")
		}
	}
}
