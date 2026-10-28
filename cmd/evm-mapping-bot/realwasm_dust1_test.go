//go:build evm_realwasm

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

// DUST-1 on the bridge wasm: a deposit below a tenth of its asset's minimum
// withdrawal is refused and not credited (BTC: MinDepositSats); one at the
// floor is credited. The ETH case pins the bot's minETHDeposit to the
// bridge's MinETHDeposit.
func TestRealWasm_DUST1_ETHBelowTheFloorIsNotCredited(t *testing.T) {
	for _, c := range []struct {
		value    *big.Int
		credited bool
	}{
		{new(big.Int).Sub(minETHDeposit, big.NewInt(1)), false},
		{minETHDeposit, true},
	} {
		ct := rwContract(t)
		_, vault := rwVaultKey(t, ct)
		userKey, _ := ethCrypto.GenerateKey()
		chain := &fakeChain{}
		tx := chain.addTx(t, userKey, &types.DynamicFeeTx{GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(30e9), Gas: 21000, To: &vault, Value: c.value})
		rwHeader(ct, chain)
		dep := detectedDeposit{BlockHeight: 101, TxIndex: 0, LogIndex: -1, TxHash: tx.Hash().Hex(), DepositType: "eth"}
		r := rwCall(ct, "hive:anyone", "map", buildMapPayloadFromRPC(context.Background(), chain.rpc(t), dep, dep.BlockHeight))
		bal := ct.StateGet(rwBridge, "a-"+rwDID(ethCrypto.PubkeyToAddress(userKey.PublicKey))+"-eth")
		if r.Success != c.credited || (bal != "") != c.credited {
			t.Fatalf("%s wei: map success=%v (%s), balance %q; want credited=%v", c.value, r.Success, r.ErrMsg, bal, c.credited)
		}
		if !c.credited && !strings.Contains(r.ErrMsg, "below the minimum") {
			t.Fatalf("%s wei refused for %q, want the minimum deposit", c.value, r.ErrMsg)
		}
	}
}

// USDC registered with min_withdrawal 1,000,000: its floor is 100,000.
func TestRealWasm_DUST1_TokenBelowTheFloorIsNotCredited(t *testing.T) {
	for _, c := range []struct {
		amount   int64
		credited bool
	}{{99_999, false}, {100_000, true}} {
		rc, _ := rwUSDCBridge(t)
		chain, dep := erc20DepositFixture(t, rc.vault, big.NewInt(c.amount))
		rwHeaderAt(rc.ct, 101, chain, rwBlockTime)
		r := rwCall(rc.ct, "hive:anyone", "map", buildMapPayloadFromRPC(context.Background(), chain.rpc(t), dep, dep.BlockHeight))
		user := rwDID(common.BytesToAddress(chain.receipts[dep.TxIndex].Logs[0].Topics[1].Bytes()))
		bal := rc.ct.StateGet(rwBridge, "a-"+user+"-usdc")
		if r.Success != c.credited || (bal != "") != c.credited {
			t.Fatalf("%d USDC units: map success=%v (%s), balance %q; want credited=%v", c.amount, r.Success, r.ErrMsg, bal, c.credited)
		}
	}
}

// MagiDeposit: an ETH or token Deposit log below the floor is refused, and the
// Transfer that paid the token one cannot be mapped as a plain ERC-20 deposit
// instead (it paid a MagiDeposit deposit).
func TestRealWasm_DUST1_MagiDepositBelowTheFloorIsNotCredited(t *testing.T) {
	c, _ := rwUSDCBridge(t)
	r := rwCall(c.ct, "hive:anyone", "depositAddress", []byte(`{}`))
	dc := common.HexToAddress(r.Ret)
	userKey, _ := ethCrypto.GenerateKey()
	sender := ethCrypto.PubkeyToAddress(userKey.PublicKey)
	depositData := func(amount *big.Int, recipient string) []byte {
		args, _ := abiStringArgs(amount, recipient)
		return args
	}
	sig := ethCrypto.Keccak256Hash([]byte("Deposit(address,address,uint256,string)"))
	usdc := common.HexToAddress(rwUSDC)
	transferSig := ethCrypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))
	ethDust := new(big.Int).Sub(minETHDeposit, big.NewInt(1))
	chain := &fakeChain{}
	chain.addTx(t, userKey, &types.DynamicFeeTx{GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(30e9), Gas: 200_000, To: &dc, Value: ethDust},
		&types.Log{Address: dc, Topics: []common.Hash{sig, common.BytesToHash(sender.Bytes()), {}}, Data: depositData(ethDust, "hive:bob")},
		&types.Log{Address: usdc, Topics: []common.Hash{transferSig, common.BytesToHash(sender.Bytes()), common.BytesToHash(c.vault.Bytes())}, Data: common.LeftPadBytes(big.NewInt(99_999).Bytes(), 32)},
		&types.Log{Address: dc, Topics: []common.Hash{sig, common.BytesToHash(sender.Bytes()), common.BytesToHash(usdc.Bytes())}, Data: depositData(big.NewInt(99_999), "hive:bob")},
	)
	rwHeaderAt(c.ct, 105, chain, rwBlockTime)
	bp, err := fetchBlockProofs(context.Background(), chain.rpc(t), 105, false)
	if err != nil {
		t.Fatal(err)
	}
	_, nodes, err := bp.receiptProof(0)
	if err != nil {
		t.Fatal(err)
	}
	mapLog := func(kind string, li int) (bool, string) {
		p := map[string]any{"block_height": 105, "tx_index": 0, "log_index": li, "deposit_type": kind}
		if kind == "contract" {
			p["receipt_proof"] = base64.StdEncoding.EncodeToString(joinNodes(nodes))
		} else {
			p["token_address"] = rwUSDC
			p["receipt_proof"] = base64.StdEncoding.EncodeToString(joinNodes(nodes))
		}
		raw, _ := json.Marshal(map[string]any{"tx_data": p})
		res := rwCall(c.ct, "hive:anyone", "map", raw)
		return res.Success, res.ErrMsg
	}
	if ok, msg := mapLog("contract", 0); ok || !strings.Contains(msg, "below the minimum") {
		t.Fatalf("MagiDeposit ETH %s wei: success=%v %q; want refused below the minimum", ethDust, ok, msg)
	}
	if ok, msg := mapLog("contract", 2); ok || !strings.Contains(msg, "below the minimum") {
		t.Fatalf("MagiDeposit 99,999 USDC units: success=%v %q; want refused below the minimum", ok, msg)
	}
	if ok, msg := mapLog("erc20", 1); ok {
		t.Fatalf("the Transfer that paid the dust MagiDeposit deposit was credited as a plain ERC-20 deposit (%q)", msg)
	}
	if got := c.ct.StateGet(rwBridge, "a-hive:bob-eth") + c.ct.StateGet(rwBridge, "a-hive:bob-usdc") + c.ct.StateGet(rwBridge, "a-"+rwDID(sender)+"-usdc"); got != "" {
		t.Fatalf("dust credited somewhere: %q", got)
	}
}
