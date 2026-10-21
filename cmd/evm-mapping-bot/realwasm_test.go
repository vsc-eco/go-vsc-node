//go:build evm_realwasm

package main

// Bot -> bridge contract, end to end on the node's wasm runtime: the payloads
// come from the bot's own builders (fed by wire_test.go's fake Ethereum
// RPC) and run against the compiled evm-mapping-contract with a verified
// header stored in a stand-in verifier contract.
//
// The bridge wasm is built from vsc-eco/account-mapping (evm-mapping-contract)
// and is not in this repo:
//
//	EVM_MAPPING_WASM=/path/to/main.wasm go test -tags evm_realwasm ./cmd/evm-mapping-bot/ -run RealWasm -v
//
// It needs libwasmedge (`. ~/.wasmedge/env`). A bridge wasm that imports
// crypto.ecrecover_strict / crypto.ecrecover_canonical fails every call on
// this node with "unknown import".

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"math/big"
	"os"
	"strings"
	"testing"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/db/vsc/contracts"
	stateEngine "vsc-node/modules/state-processing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

const (
	rwBridge   = "vsc1RWbridge"
	rwVerifier = "vsc1RWverifier"
	rwRelayer  = "hive:relayer"
)

func rwContract(t *testing.T) *test_utils.ContractTest {
	t.Helper()
	path := os.Getenv("EVM_MAPPING_WASM")
	if path == "" {
		t.Skip("EVM_MAPPING_WASM not set")
	}
	wasm, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ct := test_utils.NewContractTest()
	ct.RegisterContract(rwBridge, "hive:owner", wasm)
	ct.StateSet(rwBridge, "chainid", "11155111")
	ct.StateSet(rwBridge, "zkverifier", rwVerifier)
	return &ct
}

// rwHeader stores block 101 in the verifier's v1 header encoding
// (evm-mapping-contract contract/blocklist/blocks.go Serialize).
func rwHeader(ct *test_utils.ContractTest, c *fakeChain) {
	b := []byte{0x01}
	u := func(v uint64) { b = binary.BigEndian.AppendUint64(b, v) }
	u(101)
	b = append(b, make([]byte, 32)...) // state root (unused here)
	b = append(b, c.txRoot().Bytes()...)
	b = append(b, c.receiptRoot().Bytes()...)
	u(1e9)
	u(30_000_000)
	u(1_700_000_000)
	u(wireChainID)
	ct.StateSet(rwVerifier, "b-101", string(b))
	ct.StateSet(rwVerifier, "h", "101")
}

func rwCall(ct *test_utils.ContractTest, caller, action string, payload []byte) test_utils.ContractTestCallResult {
	return ct.Call(stateEngine.TxVscCallContract{
		Self: stateEngine.TxSelf{
			TxId: "rwtx", BlockId: "block:rw", Index: 1, Timestamp: "2026-10-18T00:00:00",
			RequiredAuths: []string{caller}, RequiredPostingAuths: []string{},
		},
		ContractId: rwBridge, Action: action, Payload: payload,
		RcLimit: 10000, Intents: []contracts.Intent{}, Caller: caller,
	})
}

// rwDID is the identity the bridge credits for an L1 address: the node's
// did:pkh:eip155:1: namespace with the EIP-55 address, on every L1 chain
// (account-mapping C6/E21).
func rwDID(a common.Address) string {
	return "did:pkh:eip155:1:" + a.Hex()
}

// confirmSpend for the bot-built payload closes the withdrawal: both proofs,
// the vault signature (strict low-S, in the contract), the intent fields.
func TestRealWasm_ConfirmSpendClosesWithdrawal(t *testing.T) {
	for _, s := range []string{"1000000000000000000", "10000000000000000000", "99999999999999999999999999"} {
		amt, _ := new(big.Int).SetString(s, 10)
		ct := rwContract(t)
		c, stored := withdrawalFixture(t, amt)
		rwHeader(ct, c)
		ct.StateSet(rwBridge, "d-0", stored)
		ct.StateSet(rwBridge, "np", "1")
		ps := parsePendingSpend(0, stored)
		if ps == nil {
			t.Fatalf("%s wei: bot refused the stored PendingSpend", s)
		}
		payload, err := buildConfirmSpendPayload(c.rpc(t), c.txs[0].Hash().Hex(), 101, 0, ps)
		if err != nil {
			t.Fatal(err)
		}
		r := rwCall(ct, rwRelayer, "confirmSpend", payload)
		if !r.Success {
			t.Fatalf("%s wei: confirmSpend failed: %s: %s", s, r.Err, r.ErrMsg)
		}
		if n := ct.StateGet(rwBridge, "n"); n != "1" {
			t.Fatalf("%s wei: confirmed nonce %q, want 1", s, n)
		}
		if ct.StateGet(rwBridge, "d-0") != "" {
			t.Fatalf("%s wei: pending spend not cleared", s)
		}
	}
}

// A vault signature with a high S value is refused by confirmSpend.
func TestRealWasm_ConfirmSpendRefusesHighSVaultSignature(t *testing.T) {
	ct := rwContract(t)
	amount := big.NewInt(1_000_000_000_000_000_000)
	vaultKey, _ := ethCrypto.GenerateKey()
	to := common.HexToAddress("0x00000000000000000000000000000000000000e2")
	signer := types.LatestSignerForChainID(big.NewInt(wireChainID))
	inner := &types.DynamicFeeTx{ChainID: big.NewInt(wireChainID), GasTipCap: big.NewInt(2e9), GasFeeCap: big.NewInt(40e9), Gas: 21000, To: &to, Value: amount}
	sig, _ := ethCrypto.Sign(signer.Hash(types.NewTx(inner)).Bytes(), vaultKey)
	new(big.Int).Sub(ethCrypto.S256().Params().N, new(big.Int).SetBytes(sig[32:64])).FillBytes(sig[32:64])
	sig[64] ^= 1
	tx, err := types.NewTx(inner).WithSignature(signer, sig)
	if err != nil {
		t.Fatal(err)
	}
	rc := &types.Receipt{Type: types.DynamicFeeTxType, Status: types.ReceiptStatusSuccessful, CumulativeGasUsed: 21000, TxHash: tx.Hash()}
	rc.Bloom = types.CreateBloom(types.Receipts{rc})
	c := &fakeChain{txs: []*types.Transaction{tx}, receipts: []*types.Receipt{rc}}
	rwHeader(ct, c)
	stored, _ := json.Marshal(contractPendingSpend{
		Amount: amount, From: "hive:alice", To: to.Hex(), Asset: "eth", UnsignedTxHex: "02ee",
		BlockHeight: 100, VaultAtQueue: ethCrypto.PubkeyToAddress(vaultKey.PublicKey).Hex(),
	})
	ct.StateSet(rwBridge, "d-0", string(stored))
	ct.StateSet(rwBridge, "np", "1")
	payload, err := buildConfirmSpendPayload(c.rpc(t), tx.Hash().Hex(), 101, 0, parsePendingSpend(0, string(stored)))
	if err != nil {
		t.Fatal(err)
	}
	r := rwCall(ct, rwRelayer, "confirmSpend", payload)
	if r.Success || !strings.Contains(r.ErrMsg, "high-S") {
		t.Fatalf("high-S vault signature: success=%v err=%s: %s", r.Success, r.Err, r.ErrMsg)
	}
	if ct.StateGet(rwBridge, "n") != "" || ct.StateGet(rwBridge, "d-0") == "" {
		t.Fatal("a refused confirmSpend changed the withdrawal state")
	}
}

// An ERC-20 deposit built by the bot is proven and credited.
func TestRealWasm_ERC20DepositIsCredited(t *testing.T) {
	ct := rwContract(t)
	vault := common.HexToAddress("0x00000000000000000000000000000000000000fa")
	c, dep := erc20DepositFixture(t, vault, big.NewInt(25_000_000))
	rwHeader(ct, c)
	ct.StateSet(rwBridge, "vault", strings.ToLower(vault.Hex()))
	ct.StateSet(rwBridge, "t-11155111-"+strings.ToLower(strings.TrimPrefix(dep.TokenAddress, "0x")), "USDC|6|1000")
	payload := buildMapPayloadFromRPC(context.Background(), c.rpc(t), dep, dep.BlockHeight)
	r := rwCall(ct, rwRelayer, "map", payload)
	if !r.Success {
		t.Fatalf("map failed: %s: %s", r.Err, r.ErrMsg)
	}
	user := common.BytesToAddress(c.receipts[dep.TxIndex].Logs[0].Topics[1].Bytes())
	if got := ct.StateGet(rwBridge, "a-"+rwDID(user)+"-USDC"); got != "25000000" {
		t.Fatalf("depositor balance %q, want 25000000", got)
	}
}

// A native ETH deposit built by the bot is proven (sender recovered with the
// contract's canonical ecrecover) and credited net of the 1% reserve tax.
func TestRealWasm_ETHDepositIsCredited(t *testing.T) {
	ct := rwContract(t)
	vaultKey, _ := ethCrypto.GenerateKey()
	vault := ethCrypto.PubkeyToAddress(vaultKey.PublicKey)
	userKey, _ := ethCrypto.GenerateKey()
	c := &fakeChain{}
	tx := c.addTx(t, userKey, &types.DynamicFeeTx{
		GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(30e9), Gas: 21000, To: &vault, Value: big.NewInt(1e18),
	})
	rwHeader(ct, c)
	ct.StateSet(rwBridge, "vault", strings.ToLower(vault.Hex()))
	ct.StateSet(rwBridge, "rl-"+rwRelayer, "1")
	dep := detectedDeposit{BlockHeight: 101, TxIndex: 0, LogIndex: -1, TxHash: tx.Hash().Hex(), DepositType: "eth"}
	payload := buildMapPayloadFromRPC(context.Background(), c.rpc(t), dep, dep.BlockHeight)
	r := rwCall(ct, rwRelayer, "map", payload)
	if !r.Success {
		t.Fatalf("map failed: %s: %s", r.Err, r.ErrMsg)
	}
	user := ethCrypto.PubkeyToAddress(userKey.PublicKey)
	if got := ct.StateGet(rwBridge, "a-"+rwDID(user)+"-eth"); got != "990000000000000000" {
		t.Fatalf("depositor balance %q, want 990000000000000000 (1 ETH less the 1%% reserve tax)", got)
	}
}
