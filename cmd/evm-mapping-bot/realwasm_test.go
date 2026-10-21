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
	"crypto/ecdsa"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/db/vsc/contracts"
	tss_db "vsc-node/modules/db/vsc/tss"
	stateEngine "vsc-node/modules/state-processing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
	gethrlp "github.com/ethereum/go-ethereum/rlp"
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

// rwVaultKey gives the bridge a TSS primary key and returns it with its
// address, which is the vault (A18).
func rwVaultKey(t *testing.T, ct *test_utils.ContractTest) (*ecdsa.PrivateKey, common.Address) {
	t.Helper()
	key, _ := ethCrypto.GenerateKey()
	ct.Tss.Keys.SetKey(tss_db.TssKey{Id: rwBridge + "-primary", Status: tss_db.TssKeyActive, Algo: tss_db.EcdsaType,
		PublicKey: hex.EncodeToString(ethCrypto.CompressPubkey(&key.PublicKey))})
	return key, ethCrypto.PubkeyToAddress(key.PublicKey)
}

// rwHeader stores block 101 in the verifier's v1 header encoding
// (evm-mapping-contract contract/blocklist/blocks.go Serialize).
func rwHeader(ct *test_utils.ContractTest, c *fakeChain) { rwHeaderAt(ct, 101, c, 1_700_000_000) }

// rwHeaderAt stores block n with c's roots and the given timestamp as the
// verifier's newest header.
func rwHeaderAt(ct *test_utils.ContractTest, n uint64, c *fakeChain, timestamp uint64) {
	b := []byte{0x01}
	u := func(v uint64) { b = binary.BigEndian.AppendUint64(b, v) }
	u(n)
	b = append(b, make([]byte, 32)...) // state root (unused here)
	b = append(b, c.txRoot().Bytes()...)
	b = append(b, c.receiptRoot().Bytes()...)
	u(1e9)
	u(30_000_000)
	u(timestamp)
	u(wireChainID)
	ct.StateSet(rwVerifier, "b-"+strconv.FormatUint(n, 10), string(b))
	ct.StateSet(rwVerifier, "h", strconv.FormatUint(n, 10))
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
	_, vault := rwVaultKey(t, ct)
	c, dep := erc20DepositFixture(t, vault, big.NewInt(25_000_000))
	rwHeader(ct, c)
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
	_, vault := rwVaultKey(t, ct)
	userKey, _ := ethCrypto.GenerateKey()
	c := &fakeChain{}
	tx := c.addTx(t, userKey, &types.DynamicFeeTx{
		GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(30e9), Gas: 21000, To: &vault, Value: big.NewInt(1e18),
	})
	rwHeader(ct, c)
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

// A18: the bridge credits only deposits to its TSS key's address. An address
// set as "vault" in state (what the removed setVault wrote) is not the vault:
// the bridge cannot spend from it, so a deposit there must not be credited.
func TestRealWasm_A18_DepositToAnAddressThatIsNotTheKeysIsRefused(t *testing.T) {
	ct := rwContract(t)
	rwVaultKey(t, ct)
	notTheKey := common.HexToAddress("0x00000000000000000000000000000000000000fa")
	ct.StateSet(rwBridge, "vault", strings.ToLower(notTheKey.Hex()))
	ct.StateSet(rwBridge, "rl-"+rwRelayer, "1")
	userKey, _ := ethCrypto.GenerateKey()
	c := &fakeChain{}
	tx := c.addTx(t, userKey, &types.DynamicFeeTx{
		GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(30e9), Gas: 21000, To: &notTheKey, Value: big.NewInt(1e18),
	})
	rwHeader(ct, c)
	dep := detectedDeposit{BlockHeight: 101, TxIndex: 0, LogIndex: -1, TxHash: tx.Hash().Hex(), DepositType: "eth"}
	r := rwCall(ct, rwRelayer, "map", buildMapPayloadFromRPC(context.Background(), c.rpc(t), dep, dep.BlockHeight))
	user := ethCrypto.PubkeyToAddress(userKey.PublicKey)
	if r.Success || ct.StateGet(rwBridge, "a-"+rwDID(user)+"-eth") != "" {
		t.Fatalf("a deposit to %s (not the TSS key's address) was credited: success=%v balance=%q", notTheKey.Hex(), r.Success, ct.StateGet(rwBridge, "a-"+rwDID(user)+"-eth"))
	}
}

// D3: a withdrawal that never mines blocked the single queue for good (every
// escape needed proof that it mined and reverted). rollWithdrawal re-signs the
// nonce as a 0-value vault self-transfer once the verified L1 chain is 1200
// blocks past it; when that mines, confirmSpend refunds the withdrawer.
func TestRealWasm_D3_StuckWithdrawalIsRolledBack(t *testing.T) {
	ct := rwContract(t)
	vaultKey, vault := rwVaultKey(t, ct)
	to := common.HexToAddress("0x00000000000000000000000000000000000000e2")
	stored, _ := json.Marshal(contractPendingSpend{
		Amount: big.NewInt(1e18), From: "hive:alice", To: to.Hex(), Asset: "eth", UnsignedTxHex: "02ee",
		BlockHeight: 100, VaultAtQueue: vault.Hex(),
	})
	ct.StateSet(rwBridge, "d-0", string(stored))
	ct.StateSet(rwBridge, "np", "1")
	now := uint64(time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC).Unix()) // rwCall's block time

	rwHeaderAt(ct, 1299, &fakeChain{}, now)
	if r := rwCall(ct, "hive:carol", "rollWithdrawal", []byte(`{"nonce":0}`)); r.Success {
		t.Fatal("rolled 1199 L1 blocks after the withdrawal was queued, want refused")
	}
	rwHeaderAt(ct, 1300, &fakeChain{}, now)
	if r := rwCall(ct, "hive:carol", "rollWithdrawal", []byte(`{"nonce":0}`)); !r.Success {
		t.Fatalf("rollWithdrawal: %s: %s", r.Err, r.ErrMsg)
	}
	ps := parsePendingSpend(0, ct.StateGet(rwBridge, "d-0"))
	if ps == nil {
		t.Fatal("pending spend gone after the roll")
	}

	// Sign the roll as the TSS group would and mine it.
	raw, _ := hex.DecodeString(ps.UnsignedTxHex)
	var f struct {
		ChainID    *big.Int
		Nonce      uint64
		GasTipCap  *big.Int
		GasFeeCap  *big.Int
		Gas        uint64
		To         *common.Address
		Value      *big.Int
		Data       []byte
		AccessList types.AccessList
	}
	if len(raw) == 0 || raw[0] != 2 || gethrlp.DecodeBytes(raw[1:], &f) != nil {
		t.Fatalf("roll tx %q is not an unsigned EIP-1559 tx", ps.UnsignedTxHex)
	}
	if f.Nonce != 0 || f.To == nil || *f.To != vault || f.Value.Sign() != 0 {
		t.Fatalf("roll tx %+v, want a 0-value self-transfer from the vault at nonce 0", f)
	}
	c := &fakeChain{}
	rolled := c.addTx(t, vaultKey, &types.DynamicFeeTx{Nonce: f.Nonce, GasTipCap: f.GasTipCap, GasFeeCap: f.GasFeeCap, Gas: f.Gas, To: f.To, Value: f.Value})
	rwHeaderAt(ct, 1301, c, now)
	payload, err := buildConfirmSpendPayload(c.rpc(t), rolled.Hash().Hex(), 1301, 0, ps)
	if err != nil {
		t.Fatal(err)
	}
	if r := rwCall(ct, rwRelayer, "confirmSpend", payload); !r.Success {
		t.Fatalf("confirmSpend for the mined roll: %s: %s", r.Err, r.ErrMsg)
	}
	if got := ct.StateGet(rwBridge, "a-hive:alice-eth"); got != "1000000000000000000" {
		t.Fatalf("withdrawer balance %q, want the 1 ETH back", got)
	}
	if ct.StateGet(rwBridge, "n") != "1" || ct.StateGet(rwBridge, "d-0") != "" {
		t.Fatalf("queue not advanced: n=%q", ct.StateGet(rwBridge, "n"))
	}
}
