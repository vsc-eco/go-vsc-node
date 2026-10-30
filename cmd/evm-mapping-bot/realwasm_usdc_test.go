//go:build evm_realwasm

package main

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/db/vsc/contracts"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	stateEngine "vsc-node/modules/state-processing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
	gethrlp "github.com/ethereum/go-ethereum/rlp"
)

// USDC on the bridge wasm, with the payloads the bot builds: the token is
// registered through propose/execute, deposited, moved on L2, withdrawn, and
// the withdrawal is confirmed both when it mines and when it reverts.

// rwUSDC is the fixture token (erc20DepositFixture: Circle's Sepolia USDC).
const rwUSDC = "0x1c7d4b196cb0c7b01d743fbc6116a902379c7238"

// rwRegisterToken proposes registerToken and executes it once its timelock
// has passed, returning the execute result.
func rwRegisterToken(t *testing.T, c rwChain, addr, symbol string) (string, bool) {
	t.Helper()
	return rwRegisterTokenJSON(t, c, `{"address":"`+addr+`","symbol":"`+symbol+`","decimals":6,"min_withdrawal":1000000}`)
}

// rwRegisterTokenJSON is rwRegisterToken with the whole registerToken payload.
func rwRegisterTokenJSON(t *testing.T, c rwChain, inner string) (string, bool) {
	t.Helper()
	c.ct.Deposit("hive:owner", 1_000_000, ledgerDb.AssetHbd) // RC beyond the free allowance
	r := rwCall(c.ct, "hive:owner", "propose", []byte(`{"action":"registerToken","payload_hex":"`+hex.EncodeToString([]byte(inner))+`"}`))
	if !r.Success {
		t.Fatalf("propose registerToken %s: %s: %s", inner, r.Err, r.ErrMsg)
	}
	var resp struct {
		ProposalId uint64 `json:"proposal_id"`
		ExecHeight uint64 `json:"exec_height"`
	}
	if err := json.Unmarshal([]byte(r.Ret), &resp); err != nil || resp.ExecHeight == 0 {
		t.Fatalf("propose response %q: %v", r.Ret, err)
	}
	if resp.ExecHeight > c.ct.BlockHeight {
		c.ct.IncrementBlocks(resp.ExecHeight - c.ct.BlockHeight)
	}
	r = rwCall(c.ct, "hive:owner", "execute", []byte(`{"proposal_id":`+jsonNum(resp.ProposalId)+`}`))
	return r.ErrMsg, r.Success
}

// rwChain bundles the contract under test with its vault key.
type rwChain struct {
	ct       *test_utils.ContractTest
	vaultKey *ecdsa.PrivateKey
	vault    common.Address
}

// rwUSDCBridge is a bridge with usdc registered, a verified vault, a 0.1 ETH
// gas reserve (its ETH custody booked to match) and depositor holding 25 USDC
// on L2 from a proven deposit. It returns the depositor's identity.
func rwUSDCBridge(t *testing.T) (rwChain, string) {
	t.Helper()
	ct := rwContract(t)
	key, vault := rwVaultKey(t, ct)
	c := rwChain{ct: ct, vaultKey: key, vault: vault}
	if msg, ok := rwRegisterToken(t, c, rwUSDC, "usdc"); !ok {
		t.Fatalf("execute registerToken usdc: %s", msg)
	}
	chain, dep := erc20DepositFixture(t, vault, big.NewInt(25_000_000))
	rwHeader(ct, chain)
	if r := rwCall(ct, rwRelayer, "map", buildMapPayloadFromRPC(context.Background(), chain.rpc(t), dep, dep.BlockHeight)); !r.Success {
		t.Fatalf("map: %s: %s", r.Err, r.ErrMsg)
	}
	user := rwDID(common.BytesToAddress(chain.receipts[dep.TxIndex].Logs[0].Topics[1].Bytes()))
	if got := ct.StateGet(rwBridge, "a-"+user+"-usdc"); got != "25000000" {
		t.Fatalf("depositor usdc balance %q, want 25000000", got)
	}
	ct.StateSet(rwBridge, "gr", "100000000000000000")
	ct.StateSet(rwBridge, "s-eth", "100000000000000000|0|0|0")
	ct.Deposit(user, 1_000_000, ledgerDb.AssetHbd) // RC for an EVM identity
	return c, user
}

func rwCallLimit(ct *test_utils.ContractTest, caller, action string, payload []byte, limit uint64) test_utils.ContractTestCallResult {
	return ct.Call(stateEngine.TxVscCallContract{
		Self: stateEngine.TxSelf{
			TxId: "rwtx", BlockId: "block:rw", Index: 1, Timestamp: "2026-10-18T00:00:00",
			RequiredAuths: []string{caller}, RequiredPostingAuths: []string{},
		},
		ContractId: rwBridge, Action: action, Payload: payload,
		RcLimit: limit, Intents: []contracts.Intent{}, Caller: caller,
	})
}

// rwBlockTime is rwCall's block time, for a header fresh enough to price a
// withdrawal (LOW 10).
var rwBlockTime = uint64(time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC).Unix())

// rwUnmapUSDC queues a usdc withdrawal of amount to `to` and returns the
// stored pending spend.
func rwUnmapUSDC(t *testing.T, c rwChain, user, to string, amount string, nonce uint64) *PendingSpend {
	t.Helper()
	rwHeaderAt(c.ct, 102, &fakeChain{}, rwBlockTime)
	// An ERC-20 unmap takes about 12.2k RC on this wasm, above rwCall's 10k.
	r := rwCallLimit(c.ct, user, "unmapERC20", []byte(`{"to":"`+to+`","amount":"`+amount+`","token_address":"`+rwUSDC+`"}`), 20_000)
	if !r.Success {
		t.Fatalf("unmapERC20: %s: %s", r.Err, r.ErrMsg)
	}
	key := "d-" + jsonNum(nonce)
	ps := parsePendingSpend(nonce, c.ct.StateGet(rwBridge, key))
	if ps == nil || ps.Asset != "usdc" {
		t.Fatalf("pending spend %s = %q", key, c.ct.StateGet(rwBridge, key))
	}
	return ps
}

// rwMine signs the withdrawal's unsigned tx with the vault key, as the TSS
// group would, and mines it in L1 block n with the given receipt status.
func rwMine(t *testing.T, c rwChain, ps *PendingSpend, n uint64, status uint64) (*fakeChain, *types.Transaction) {
	t.Helper()
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
		t.Fatalf("withdrawal tx %q is not an unsigned EIP-1559 tx", ps.UnsignedTxHex)
	}
	chain := &fakeChain{}
	// A token's transfer emits its Transfer log when it succeeds (A36: the
	// bridge counts an ERC-20 withdrawal paid only with it).
	var logs []*types.Log
	if status == types.ReceiptStatusSuccessful && len(f.Data) == 68 && f.To != nil {
		logs = append(logs, &types.Log{Address: *f.To, Topics: []common.Hash{ethCrypto.Keccak256Hash([]byte("Transfer(address,address,uint256)")),
			common.BytesToHash(c.vault.Bytes()), common.BytesToHash(f.Data[16:36])}, Data: f.Data[36:68]})
	}
	tx := chain.addTx(t, c.vaultKey, &types.DynamicFeeTx{Nonce: f.Nonce, GasTipCap: f.GasTipCap, GasFeeCap: f.GasFeeCap, Gas: f.Gas, To: f.To, Value: f.Value, Data: f.Data}, logs...)
	chain.receipts[0].Status = status
	rwHeaderAt(c.ct, n, chain, rwBlockTime)
	return chain, tx
}

func rwConfirm(t *testing.T, c rwChain, chain *fakeChain, tx *types.Transaction, n uint64, ps *PendingSpend) {
	t.Helper()
	payload, err := buildConfirmSpendPayload(context.Background(), chain.rpc(t), tx.Hash().Hex(), n, 0, ps)
	if err != nil {
		t.Fatal(err)
	}
	// At rwCall's 10k RC, the bot's default RC_LIMIT (an ERC-20 confirmSpend
	// took about 2k).
	if r := rwCall(c.ct, rwRelayer, "confirmSpend", payload); !r.Success {
		t.Fatalf("confirmSpend: %s: %s", r.Err, r.ErrMsg)
	}
}

func rwExpect(t *testing.T, ct *test_utils.ContractTest, key, want string) {
	t.Helper()
	if got := ct.StateGet(rwBridge, key); got != want {
		t.Fatalf("%s = %q, want %q", key, got, want)
	}
}

// The USDC path end to end: an L2 transfer of a registered token passes the
// asset rule (E1), unmapERC20 queues a transfer(recipient, amount) on the
// token from the vault, and confirmSpend for the mined transfer closes it.
// Custody and the reserve move once, at unmap.
func TestRealWasm_USDC_WithdrawalLifecycle(t *testing.T) {
	c, user := rwUSDCBridge(t)
	other := "did:pkh:eip155:1:0x00000000000000000000000000000000000000D1"

	if r := rwCall(c.ct, user, "transfer", []byte(`{"to":"`+other+`","amount":"5000000","asset":"usdc"}`)); !r.Success {
		t.Fatalf("L2 transfer of usdc: %s: %s", r.Err, r.ErrMsg)
	}
	rwExpect(t, c.ct, "a-"+other+"-usdc", "5000000")

	to := "0x00000000000000000000000000000000000000e2"
	ps := rwUnmapUSDC(t, c, user, to, "12000000", 0)
	if !strings.EqualFold(ps.TokenAddress, rwUSDC) || ps.Amount.String() != "12000000" {
		t.Fatalf("pending spend %+v", ps)
	}
	rwExpect(t, c.ct, "a-"+user+"-usdc", "8000000")
	rwExpect(t, c.ct, "s-usdc", "13000000|13000000|0|0")
	// 65,000 gas at 2 x 1 gwei base fee + 2 gwei tip, from the reserve.
	rwExpect(t, c.ct, "gr", "99740000000000000")
	rwExpect(t, c.ct, "s-eth", "99740000000000000|0|0|0")

	chain, tx := rwMine(t, c, ps, 103, types.ReceiptStatusSuccessful)
	if tx.To() == nil || !strings.EqualFold(tx.To().Hex(), rwUSDC) || tx.Value().Sign() != 0 {
		t.Fatalf("withdrawal tx goes to %v with value %s, want the token with 0", tx.To(), tx.Value())
	}
	rwConfirm(t, c, chain, tx, 103, ps)
	rwExpect(t, c.ct, "n", "1")
	rwExpect(t, c.ct, "d-0", "")
	rwExpect(t, c.ct, "a-"+user+"-usdc", "8000000")
	rwExpect(t, c.ct, "s-usdc", "13000000|13000000|0|0")
	rwExpect(t, c.ct, "gr", "99740000000000000")
}

// USDC-1: a usdc transfer to a Circle-blacklisted address reverts on L1 (USDC
// blacklists its own contract address, so one always exists). confirmSpend
// refunds the withdrawer in full, while the gas reserve paid the mined revert.
// Nothing stops the withdrawer from doing it again: each round costs the
// reserve one ERC-20 transfer's gas and the withdrawer nothing on L1.
func TestRealWasm_USDC1_RevertedWithdrawalRefundsTheTokensAndTheReservePays(t *testing.T) {
	c, user := rwUSDCBridge(t)
	for round, n := range []uint64{103, 104} {
		ps := rwUnmapUSDC(t, c, user, rwUSDC, "25000000", uint64(round))
		rwExpect(t, c.ct, "a-"+user+"-usdc", "0")
		chain, tx := rwMine(t, c, ps, n, types.ReceiptStatusFailed)
		rwConfirm(t, c, chain, tx, n, ps)
		rwExpect(t, c.ct, "n", jsonNum(uint64(round+1)))
		rwExpect(t, c.ct, "a-"+user+"-usdc", "25000000")
		rwExpect(t, c.ct, "s-usdc", "25000000|25000000|0|0")
	}
	// Two rounds of 65,000 gas x 4 gwei: the reserve and its ETH custody paid both.
	rwExpect(t, c.ct, "gr", "99480000000000000")
	rwExpect(t, c.ct, "s-eth", "99480000000000000|0|0|0")
}

// M15-N5 on the bridge wasm: a second token under a registered symbol is
// refused at execute, and its deposits are not credited.
func TestRealWasm_M15N5_SecondTokenUnderOneSymbolIsRefused(t *testing.T) {
	ct := rwContract(t)
	key, vault := rwVaultKey(t, ct)
	c := rwChain{ct: ct, vaultKey: key, vault: vault}
	fake := "0x00000000000000000000000000000000000000bb"
	if msg, ok := rwRegisterToken(t, c, fake, "usdc"); !ok {
		t.Fatalf("first registration: %s", msg)
	}
	msg, ok := rwRegisterToken(t, c, rwUSDC, "usdc")
	if ok || !strings.Contains(msg, "symbol usdc belongs to token") {
		t.Fatalf("second token under usdc: success=%v %s", ok, msg)
	}
	if ct.StateGet(rwBridge, "t-11155111-"+strings.TrimPrefix(rwUSDC, "0x")) != "" {
		t.Fatal("the refused token is in the registry")
	}
	chain, dep := erc20DepositFixture(t, vault, big.NewInt(25_000_000))
	rwHeader(ct, chain)
	if r := rwCall(ct, rwRelayer, "map", buildMapPayloadFromRPC(context.Background(), chain.rpc(t), dep, dep.BlockHeight)); r.Success {
		t.Fatal("a deposit of the refused token was credited")
	}
}
