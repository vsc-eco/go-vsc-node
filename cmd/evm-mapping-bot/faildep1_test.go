package main

import (
	"encoding/json"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/core/types"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

// FAILDEP-1: a transfer to the vault that reverted on L1 moved nothing, so the
// scan does not report it as a deposit; one that succeeded is reported.
func TestFAILDEP1_ScanSkipsARevertedVaultTransfer(t *testing.T) {
	vaultKey, _ := ethCrypto.GenerateKey()
	vault := ethCrypto.PubkeyToAddress(vaultKey.PublicKey)
	userKey, _ := ethCrypto.GenerateKey()
	c := &fakeChain{}
	reverted := c.addTx(t, userKey, &types.DynamicFeeTx{Nonce: 0, GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(30e9), Gas: 21000, To: &vault, Value: big.NewInt(5e18)})
	c.receipts[0].Status = types.ReceiptStatusFailed
	ok := c.addTx(t, userKey, &types.DynamicFeeTx{Nonce: 1, GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(30e9), Gas: 250000, To: &vault, Value: big.NewInt(1e18)})
	res, err := scanBlock(c.rpc(t), 101, []string{vault.Hex()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Deposits) != 1 || res.Deposits[0].TxHash != ok.Hash().Hex() {
		t.Fatalf("deposits %+v: want only %s (the reverted %s moved nothing)", res.Deposits, ok.Hash().Hex(), reverted.Hash().Hex())
	}
}

// The ETH map payload carries the receipt proof the bridge checks.
func TestFAILDEP1_ETHMapPayloadCarriesTheReceiptProof(t *testing.T) {
	vaultKey, _ := ethCrypto.GenerateKey()
	vault := ethCrypto.PubkeyToAddress(vaultKey.PublicKey)
	userKey, _ := ethCrypto.GenerateKey()
	c := &fakeChain{}
	tx := c.addTx(t, userKey, &types.DynamicFeeTx{GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(30e9), Gas: 21000, To: &vault, Value: big.NewInt(1e18)})
	dep := detectedDeposit{BlockHeight: 101, TxIndex: 0, LogIndex: -1, TxHash: tx.Hash().Hex(), DepositType: "eth"}
	var p struct {
		TxData struct {
			ReceiptProof string `json:"receipt_proof"`
		} `json:"tx_data"`
	}
	if err := json.Unmarshal(buildMapPayloadFromRPC(t.Context(), c.rpc(t), dep, 101), &p); err != nil || p.TxData.ReceiptProof == "" {
		t.Fatalf("no receipt_proof in the eth map payload (%v)", err)
	}
}
