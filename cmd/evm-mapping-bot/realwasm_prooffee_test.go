//go:build evm_realwasm

package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	ledgerDb "vsc-node/modules/db/vsc/ledger"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

// rwSetProofFee proposes and executes setProofFee on the bridge wasm.
func rwSetProofFee(t *testing.T, c rwChain, asset, amount string) (string, bool) {
	t.Helper()
	c.ct.Deposit("hive:owner", 1_000_000, ledgerDb.AssetHbd)
	inner := `{"asset":"` + asset + `","amount":"` + amount + `"}`
	r := rwCall(c.ct, "hive:owner", "propose", []byte(`{"action":"setProofFee","payload_hex":"`+hex.EncodeToString([]byte(inner))+`"}`))
	if !r.Success {
		t.Fatalf("propose setProofFee %s: %s: %s", inner, r.Err, r.ErrMsg)
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

// The proving fee on the bridge wasm, with the bot's own payloads: set through
// propose/execute, a plain ETH deposit and an ERC-20 deposit each pay it out of
// their credit, an ETH and a USDC withdrawal each pay it on top, all to
// hive:altera.app; a fee above half the minimum deposit is refused.
func TestRealWasm_PROOFFEE_EveryProvenOperationPaysAltera(t *testing.T) {
	c, holder := rwUSDCBridge(t) // holder has 25 USDC, mapped before any fee
	if msg, ok := rwSetProofFee(t, c, "eth", "100000000000000"); !ok {
		t.Fatalf("setProofFee eth: %s", msg)
	}
	// usdc is registered here with a 1 USDC minimum withdrawal: its floor is
	// 0.1 USDC, so at most 0.05 USDC.
	if msg, ok := rwSetProofFee(t, c, "usdc", "100000"); ok || !strings.Contains(msg, "above half") {
		t.Fatalf("a usdc fee above half its floor: ok=%v %q, want refused", ok, msg)
	}
	if msg, ok := rwSetProofFee(t, c, "usdc", "40000"); !ok {
		t.Fatalf("setProofFee usdc: %s", msg)
	}
	if msg, ok := rwSetProofFee(t, c, "eth", "500000000000001"); ok || !strings.Contains(msg, "above half") {
		t.Fatalf("a fee above half the 0.001 ETH floor: ok=%v %q, want refused", ok, msg)
	}
	altera := func(asset string) string { return c.ct.StateGet(rwBridge, "a-hive:altera.app-"+asset) }
	ctx := context.Background()

	// A plain ETH deposit of 6 ETH: 1% to the reserve, the fee to altera.app.
	userKey, _ := ethCrypto.GenerateKey()
	chain := &fakeChain{}
	six := new(big.Int).Mul(big.NewInt(6), big.NewInt(1e18))
	tx := chain.addTx(t, userKey, &types.DynamicFeeTx{GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(30e9), Gas: 21000, To: &c.vault, Value: six})
	rwHeaderAt(c.ct, 103, chain, rwBlockTime)
	dep := detectedDeposit{BlockHeight: 103, TxIndex: 0, LogIndex: -1, TxHash: tx.Hash().Hex(), DepositType: "eth"}
	if r := rwCall(c.ct, "hive:anyone", "map", buildMapPayloadFromRPC(ctx, chain.rpc(t), dep, 103)); !r.Success {
		t.Fatalf("ETH map: %s: %s", r.Err, r.ErrMsg)
	}
	ethUser := rwDID(ethCrypto.PubkeyToAddress(userKey.PublicKey))
	if got := c.ct.StateGet(rwBridge, "a-"+ethUser+"-eth"); got != "5939900000000000000" || altera("eth") != "100000000000000" {
		t.Fatalf("ETH deposit: user %q, altera %q; want 5.94 ETH less 1e14, and 1e14", got, altera("eth"))
	}

	// An ERC-20 deposit of 5 USDC.
	chain2, dep2 := erc20DepositFixture(t, c.vault, big.NewInt(5_000_000))
	rwHeaderAt(c.ct, 101, chain2, rwBlockTime)
	if r := rwCall(c.ct, "hive:anyone", "map", buildMapPayloadFromRPC(ctx, chain2.rpc(t), dep2, dep2.BlockHeight)); !r.Success {
		t.Fatalf("ERC-20 map: %s: %s", r.Err, r.ErrMsg)
	}
	tokenUser := rwDID(common.BytesToAddress(chain2.receipts[dep2.TxIndex].Logs[0].Topics[1].Bytes()))
	if got := c.ct.StateGet(rwBridge, "a-"+tokenUser+"-usdc"); got != "4960000" || altera("usdc") != "40000" {
		t.Fatalf("USDC deposit: user %q, altera %q; want 4960000 and 40000", got, altera("usdc"))
	}

	// Withdrawals, priced from a fresh header.
	rwHeaderAt(c.ct, 106, &fakeChain{}, rwBlockTime)
	c.ct.Deposit(ethUser, 1_000_000, ledgerDb.AssetHbd)
	before, _ := new(big.Int).SetString(c.ct.StateGet(rwBridge, "a-"+ethUser+"-eth"), 10)
	r := rwCallLimit(c.ct, ethUser, "unmapETH", []byte(`{"to":"0x00000000000000000000000000000000000000e2","amount":"1000000000000000000","asset":"eth"}`), 50_000)
	if !r.Success {
		t.Fatalf("unmapETH: %s: %s", r.Err, r.ErrMsg)
	}
	t.Logf("unmapETH with the proving fee: %d RC", r.RcUsed)
	after, _ := new(big.Int).SetString(c.ct.StateGet(rwBridge, "a-"+ethUser+"-eth"), 10)
	if paid := new(big.Int).Sub(before, after); paid.Cmp(big.NewInt(1e18+1e14)) <= 0 || altera("eth") != "200000000000000" {
		t.Fatalf("unmapETH: user paid %s (want 1 ETH + its L1 fee + 1e14), altera %q (want 2e14)", paid, altera("eth"))
	}
	r = rwCallLimit(c.ct, holder, "unmapERC20", []byte(`{"to":"0x00000000000000000000000000000000000000e3","amount":"2000000","token_address":"`+rwUSDC+`"}`), 20_000)
	if !r.Success {
		t.Fatalf("unmapERC20: %s: %s", r.Err, r.ErrMsg)
	}
	if got := c.ct.StateGet(rwBridge, "a-"+holder+"-usdc"); got != "22960000" || altera("usdc") != "80000" {
		t.Fatalf("unmapERC20: holder %q, altera %q; want 22960000 and 80000", got, altera("usdc"))
	}
	t.Logf("unmapERC20 with the proving fee: %d RC", r.RcUsed)
}
