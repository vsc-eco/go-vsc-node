package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"testing"

	"vsc-node/modules/common/params"

	"github.com/ethereum/go-ethereum/core/types"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

// ETH-PAYLOAD-1: an ETH deposit's map payload sent the tx twice in hex, so a
// deposit tx above ~600 bytes in a busy block was over the node's 8,192-byte
// cap and was never mapped (no recovery: its ETH stayed in the vault). Over the
// cap the bot now sends the tx-trie nodes once in base64 (tx_proof, the leaf is
// the tx); under the cap the payload is unchanged, so a bridge wasm without
// tx_proof keeps mapping every deposit it mapped before.
func TestETHPAYLOAD1_OverTheLegacyCapSendsTxProof(t *testing.T) {
	vaultKey, _ := ethCrypto.GenerateKey()
	vault := ethCrypto.PubkeyToAddress(vaultKey.PublicKey)
	build := func(calldata int) (*fakeChain, *types.Transaction, json.RawMessage, map[string]any) {
		c := &fakeChain{}
		var tx *types.Transaction
		for i := 0; i < 300; i++ { // a busy block, the deposit in the middle
			k, _ := ethCrypto.GenerateKey()
			to := ethCrypto.PubkeyToAddress(k.PublicKey)
			var data []byte
			if i == 150 {
				to = vault
				data = make([]byte, calldata)
			}
			added := c.addTx(t, k, &types.DynamicFeeTx{GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(30e9), Gas: 21000 + 16*uint64(len(data)), To: &to, Value: big.NewInt(1e17), Data: data})
			if i == 150 {
				tx = added
			}
		}
		dep := detectedDeposit{BlockHeight: 101, TxIndex: 150, LogIndex: -1, TxHash: tx.Hash().Hex(), DepositType: "eth"}
		payload := buildMapPayloadFromRPC(context.Background(), c.rpc(t), dep, dep.BlockHeight)
		var p struct {
			TxData map[string]any `json:"tx_data"`
		}
		if err := json.Unmarshal(payload, &p); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return c, tx, payload, p.TxData
	}

	// Under the cap: the legacy form, as before.
	c, tx, payload, td := build(0)
	raw, _ := tx.MarshalBinary()
	if td["raw_hex"] != hex.EncodeToString(raw) || td["tx_proof"] != nil {
		t.Fatalf("plain deposit: raw_hex %v tx_proof %v; want the legacy form", td["raw_hex"] != nil, td["tx_proof"] != nil)
	}
	verifyLeaf(t, "legacy eth tx", c.txRoot(), 150, td["raw_hex"].(string), td["merkle_proof_hex"].(string))

	// 1 KB of calldata: over the cap in the legacy form, sent as tx_proof.
	c, tx, payload, td = build(1000)
	raw, _ = tx.MarshalBinary()
	if td["raw_hex"] != nil || td["merkle_proof_hex"] != nil || td["tx_proof"] == nil {
		t.Fatalf("1 KB deposit: raw_hex %v merkle_proof_hex %v tx_proof %v; want only tx_proof", td["raw_hex"] != nil, td["merkle_proof_hex"] != nil, td["tx_proof"] != nil)
	}
	if n := nodePayloadSize(payload); n > params.MAX_CONTRACT_PAYLOAD_SIZE {
		t.Fatalf("1 KB deposit payload %d bytes, over the %d cap", n, params.MAX_CONTRACT_PAYLOAD_SIZE)
	}
	nodes, err := base64.StdEncoding.DecodeString(td["tx_proof"].(string))
	if err != nil {
		t.Fatal(err)
	}
	verifyLeaf(t, "tx_proof eth tx", c.txRoot(), 150, hex.EncodeToString(raw), hex.EncodeToString(nodes))
	if td["receipt_proof"] == nil {
		t.Fatal("tx_proof payload lost its receipt_proof (FAILDEP-1)")
	}
}
