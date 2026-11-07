package main

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"testing"

	"vsc-node/modules/common/params"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

// EVM-T1: every node fails a contract call whose payload is over
// params.MAX_CONTRACT_PAYLOAD_SIZE, and the withdrawal a confirmSpend settles
// is the only one the contract lets be pending, so one oversize confirmSpend
// froze every withdrawal on the bridge. With the proofs in hex and the tx and
// receipt also sent beside the proofs that already carry them, an ERC-20
// withdrawal mined late in a large block went over: three logs at index 1,200
// of a 1,500-tx block came to about 10.8 KB.
func TestEVMT1_ConfirmSpendFitsThePayloadCap(t *testing.T) {
	const txs, target = 1500, 1200
	key, _ := ethCrypto.GenerateKey()
	vault := ethCrypto.PubkeyToAddress(key.PublicKey)
	token := common.HexToAddress("0x00000000000000000000000000000000000000c0")
	to := common.HexToAddress("0x00000000000000000000000000000000000000e2")
	amount := big.NewInt(1_000_000)
	transfer := common.HexToHash(TransferEventSig)

	c := &fakeChain{}
	for i := 0; i < txs; i++ {
		inner := &types.DynamicFeeTx{
			Nonce: uint64(i), GasTipCap: big.NewInt(2e9), GasFeeCap: big.NewInt(40e9), Gas: 21000, To: &to, Value: big.NewInt(1),
		}
		if i != target {
			c.addTx(t, key, inner)
			continue
		}
		// The withdrawal: transfer(to, amount) on the token, three logs.
		data := append(common.FromHex("a9059cbb"), common.LeftPadBytes(to.Bytes(), 32)...)
		inner.Data = append(data, common.LeftPadBytes(amount.Bytes(), 32)...)
		inner.To, inner.Value, inner.Gas = &token, big.NewInt(0), 65000
		logs := make([]*types.Log, 3)
		for j := range logs {
			logs[j] = &types.Log{
				Address: token,
				Topics:  []common.Hash{transfer, common.BytesToHash(vault.Bytes()), common.BytesToHash(to.Bytes())},
				Data:    common.LeftPadBytes(amount.Bytes(), 32),
			}
		}
		c.addTx(t, key, inner, logs...)
	}

	ps := &PendingSpend{Nonce: target, To: to.Hex(), Amount: amount, Asset: "usdc", TokenAddress: token.Hex()}
	payload, err := buildConfirmSpendPayload(context.Background(), c.rpc(t), c.txs[target].Hash().Hex(), 101, target, ps)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) > params.MAX_CONTRACT_PAYLOAD_SIZE {
		t.Fatalf("confirmSpend payload is %d bytes, over the node's %d-byte contract-payload cap", len(payload), params.MAX_CONTRACT_PAYLOAD_SIZE)
	}
	t.Logf("confirmSpend payload: %d bytes (cap %d)", len(payload), params.MAX_CONTRACT_PAYLOAD_SIZE)

	// And it is still the payload the contract verifies.
	var req contractConfirmSpendRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		t.Fatal(err)
	}
	rawTx, _ := c.txs[target].MarshalBinary()
	rawReceipt, _ := c.receipts[target].MarshalBinary()
	verifyLeafB64(t, "tx leg", c.txRoot(), target, rawTx, req.TxProof)
	verifyLeafB64(t, "receipt leg", c.receiptRoot(), target, rawReceipt, req.ReceiptProof)
}

// EVM-T1: a payload over the node's cap is refused before submission, with an
// error naming the action, instead of being sent as a call every node fails.
func TestEVMT1_CallOverThePayloadCapIsRefused(t *testing.T) {
	// No GraphQL endpoints: a call that gets past the size check fails at the
	// nonce fetch instead, with a different error.
	s := &l2Submitter{gql: &vscGraphQL{}}
	// payloadOf(n) is n bytes as the node counts them (nodePayloadSize: the
	// JSON string literal, so n-2 plain characters).
	payloadOf := func(n int) json.RawMessage { return json.RawMessage(strings.Repeat("a", n-2)) }

	over := payloadOf(params.MAX_CONTRACT_PAYLOAD_SIZE + 1)
	_, err := s.callContractL2(context.Background(), "vsc1bridge", "confirmSpend", over, 0)
	if !errors.Is(err, errPayloadTooLarge) {
		t.Fatalf("payload of %d bytes to the node: err = %v, want errPayloadTooLarge", nodePayloadSize(over), err)
	}
	if !strings.Contains(err.Error(), "confirmSpend") {
		t.Fatalf("error does not name the action: %v", err)
	}

	atCap := payloadOf(params.MAX_CONTRACT_PAYLOAD_SIZE)
	if _, err := s.callContractL2(context.Background(), "vsc1bridge", "confirmSpend", atCap, 0); errors.Is(err, errPayloadTooLarge) {
		t.Fatalf("payload of exactly %d bytes to the node was refused; the node accepts it", nodePayloadSize(atCap))
	}
}
