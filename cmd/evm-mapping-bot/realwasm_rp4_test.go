//go:build evm_realwasm

package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	ledgerDb "vsc-node/modules/db/vsc/ledger"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
	gethrlp "github.com/ethereum/go-ethereum/rlp"
)

// The 10-27 launch fixes on the bridge wasm: RP-4 (no reserve floor on the
// self-funded ETH path), RP-11 (several withdrawals in flight, an L2 queue),
// USDC-1 B (a token's revert fee), GLAM-1 (an ETH withdrawal's own gas limit)
// and RP-7 (MagiDeposit deposits for an explicit recipient).

// rwETHUser maps a proven 1 ETH deposit (1% to the reserve) from a fresh key
// in L1 block 102 and gives its identity RC; it returns the identity.
func rwETHUser(t *testing.T, c rwChain) string {
	t.Helper()
	ethKey, _ := ethCrypto.GenerateKey()
	chain := &fakeChain{}
	tx := chain.addTx(t, ethKey, &types.DynamicFeeTx{
		GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(30e9), Gas: 21000, To: &c.vault, Value: big.NewInt(1e18),
	})
	rwHeaderAt(c.ct, 102, chain, rwBlockTime)
	dep := detectedDeposit{BlockHeight: 102, TxIndex: 0, LogIndex: -1, TxHash: tx.Hash().Hex(), DepositType: "eth"}
	// RP-8: any caller may map a native ETH deposit (no relayer registration).
	if r := rwCall(c.ct, "hive:anyone", "map", buildMapPayloadFromRPC(context.Background(), chain.rpc(t), dep, dep.BlockHeight)); !r.Success {
		t.Fatalf("map ETH deposit: %s: %s", r.Err, r.ErrMsg)
	}
	ethUser := rwDID(ethCrypto.PubkeyToAddress(ethKey.PublicKey))
	rwExpect(t, c.ct, "a-"+ethUser+"-eth", "990000000000000000")
	c.ct.Deposit(ethUser, 1_000_000, ledgerDb.AssetHbd) // RC for an EVM identity
	return ethUser
}

// rwPending reads pending spend d-{nonce}.
func rwPending(t *testing.T, c rwChain, nonce uint64) *PendingSpend {
	t.Helper()
	ps := parsePendingSpend(nonce, c.ct.StateGet(rwBridge, "d-"+jsonNum(nonce)))
	if ps == nil {
		t.Fatalf("no pending spend d-%d", nonce)
	}
	return ps
}

// rwTxGas is the gas limit of a pending spend's unsigned EIP-1559 tx.
func rwTxGas(t *testing.T, ps *PendingSpend) uint64 {
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
		t.Fatalf("not an unsigned EIP-1559 tx: %s", ps.UnsignedTxHex)
	}
	return f.Gas
}

// RP-4: a USDC withdrawal takes its gas from the reserve and leaves it below
// the 0.05 ETH floor; an ETH withdrawal, paid by its owner, still goes through
// and does not touch the reserve. Old code: refused "insufficient gas reserve".
func TestRealWasm_RP4_ETHWithdrawalNeedsNoReserveFloor(t *testing.T) {
	c, usdcUser := rwUSDCBridge(t)
	ethUser := rwETHUser(t, c)
	floor := "50000000000000000" // MinGasReserve
	c.ct.StateSet(rwBridge, "gr", floor)
	c.ct.StateSet(rwBridge, "s-eth", "1050000000000000000|990000000000000000|0|0") // 1 ETH deposit (0.99 user) + 0.06 reserve
	ps := rwUnmapUSDC(t, c, usdcUser, "0x00000000000000000000000000000000000000e2", "1000000", 0)
	below := c.ct.StateGet(rwBridge, "gr")
	if b, _ := new(big.Int).SetString(below, 10); b == nil || b.Cmp(big.NewInt(50_000_000_000_000_000)) >= 0 {
		t.Fatalf("reserve %s after the USDC withdrawal, want below the floor", below)
	}
	r := rwCall(c.ct, ethUser, "unmapETH", []byte(`{"to":"0x00000000000000000000000000000000000000e3","amount":"10000000000000000","asset":"eth"}`))
	if !r.Success {
		t.Fatalf("unmapETH with the reserve below the floor: %s: %s", r.Err, r.ErrMsg)
	}
	rwExpect(t, c.ct, "gr", below)
	if p := rwPending(t, c, 1); p.Asset != "eth" || p.Nonce != 1 || ps.Nonce != 0 {
		t.Fatalf("ETH withdrawal not issued at nonce 1 behind the USDC one: %+v", p)
	}
}

// RP-11: while a USDC withdrawal is in flight (nonce 0), another user's ETH
// withdrawal is issued at nonce 1; they confirm in nonce order. Old code:
// "withdrawal pending: wait for confirmation".
func TestRealWasm_RP11_SeveralWithdrawalsInFlightConfirmInOrder(t *testing.T) {
	c, usdcUser := rwUSDCBridge(t)
	ethUser := rwETHUser(t, c)
	ps0 := rwUnmapUSDC(t, c, usdcUser, "0x00000000000000000000000000000000000000e2", "1000000", 0)
	r := rwCall(c.ct, ethUser, "unmapETH", []byte(`{"to":"0x00000000000000000000000000000000000000e3","amount":"10000000000000000","asset":"eth"}`))
	if !r.Success {
		t.Fatalf("a second user's unmapETH with a withdrawal in flight: %s: %s", r.Err, r.ErrMsg)
	}
	t.Logf("unmapETH with another withdrawal in flight: %d RC", r.RcUsed)
	ps1 := rwPending(t, c, 1)
	rwExpect(t, c.ct, "np", "2")
	chain0, tx0 := rwMine(t, c, ps0, 103, types.ReceiptStatusSuccessful)
	chain1, tx1 := rwMine(t, c, ps1, 104, types.ReceiptStatusSuccessful)
	// The second cannot be confirmed before the first.
	payload, err := buildConfirmSpendPayload(chain1.rpc(t), tx1.Hash().Hex(), 104, 0, ps1)
	if err != nil {
		t.Fatal(err)
	}
	if r := rwCall(c.ct, rwRelayer, "confirmSpend", payload); r.Success {
		t.Fatal("nonce 1 confirmed while nonce 0 is the head")
	}
	// A third withdrawal waits behind a stale price; confirming the head does
	// not issue it (confirmSpend's RC stays the bot's 10k default).
	rwHeaderAt(c.ct, 105, &fakeChain{}, rwBlockTime-7200)
	if r := rwCall(c.ct, ethUser, "unmapETH", []byte(`{"to":"0x00000000000000000000000000000000000000e3","amount":"10000000000000000","asset":"eth"}`)); !r.Success || r.Ret != "queued:0" {
		t.Fatalf("third unmap: %v %q %s", r.Success, r.Ret, r.ErrMsg)
	}
	rwConfirm(t, c, chain0, tx0, 103, ps0)
	rwExpect(t, c.ct, "n", "1")
	rwExpect(t, c.ct, "np", "2")
	rwConfirm(t, c, chain1, tx1, 104, ps1)
	rwExpect(t, c.ct, "n", "2")
	rwExpect(t, c.ct, "d-1", "")
}

// RP-11: with no fresh price a withdrawal waits in the L2 queue (it is not
// refused); its owner can cancel it for a full refund, and drainWithdrawalQueue
// issues the next one once a fresh header lands, refunding the unused fee cap.
func TestRealWasm_RP11_QueueCancelAndDrain(t *testing.T) {
	c, _ := rwUSDCBridge(t)
	ethUser := rwETHUser(t, c)
	rwHeaderAt(c.ct, 103, &fakeChain{}, rwBlockTime-7200) // 2 h old: too stale to price
	unmap := []byte(`{"to":"0x00000000000000000000000000000000000000e3","amount":"10000000000000000","asset":"eth"}`)
	for i := 0; i < 2; i++ {
		r := rwCall(c.ct, ethUser, "unmapETH", unmap)
		if !r.Success || !strings.Contains(r.Ret, "queued:") {
			t.Fatalf("unmapETH with a stale header #%d: success=%v ret=%q %s", i, r.Success, r.Ret, r.ErrMsg)
		}
		t.Logf("queued unmapETH: %d RC", r.RcUsed)
	}
	rwExpect(t, c.ct, "np", "")
	// 0.99 - 2 x (0.01 + default cap 21,000 x 20 gwei = 0.00042)
	rwExpect(t, c.ct, "a-"+ethUser+"-eth", "969160000000000000")
	if r := rwCall(c.ct, ethUser, "cancelQueuedWithdrawal", []byte(`{"seq":0}`)); !r.Success {
		t.Fatalf("cancel: %s: %s", r.Err, r.ErrMsg)
	}
	rwExpect(t, c.ct, "a-"+ethUser+"-eth", "979580000000000000")
	rwHeaderAt(c.ct, 104, &fakeChain{}, rwBlockTime)
	c.ct.Deposit(rwRelayer, 1_000_000, ledgerDb.AssetHbd) // RC past a Hive account's free 10k
	r := rwCallLimit(c.ct, rwRelayer, "drainWithdrawalQueue", []byte(`{}`), 30_000)
	if !r.Success || r.Ret != "1" {
		t.Fatalf("drain: %v %q %s: %s", r.Success, r.Ret, r.Err, r.ErrMsg)
	}
	t.Logf("drainWithdrawalQueue issuing one: %d RC", r.RcUsed)
	ps := rwPending(t, c, 0)
	if ps.Asset != "eth" || rwTxGas(t, ps) != 21_000 {
		t.Fatalf("drained withdrawal %+v", ps)
	}
	// Issued at 1 gwei base: fee 21,000 x 4 gwei = 0.000084; the rest of the
	// 0.00042 cap came back.
	rwExpect(t, c.ct, "a-"+ethUser+"-eth", "979916000000000000")
}

// USDC-1 B: a reverted token withdrawal refunds the amount less the token's
// revert fee, which goes to the contract owner.
func TestRealWasm_USDC1B_RevertFeeIsKept(t *testing.T) {
	ct := rwContract(t)
	key, vault := rwVaultKey(t, ct)
	c := rwChain{ct: ct, vaultKey: key, vault: vault}
	if msg, ok := rwRegisterTokenJSON(t, c, `{"address":"`+rwUSDC+`","symbol":"usdc","decimals":6,"min_withdrawal":1000000,"revert_fee":250000}`); !ok {
		t.Fatalf("register usdc with a revert fee: %s", msg)
	}
	chain, dep := erc20DepositFixture(t, vault, big.NewInt(25_000_000))
	rwHeader(ct, chain)
	if r := rwCall(ct, rwRelayer, "map", buildMapPayloadFromRPC(context.Background(), chain.rpc(t), dep, dep.BlockHeight)); !r.Success {
		t.Fatalf("map: %s: %s", r.Err, r.ErrMsg)
	}
	user := rwDID(common.BytesToAddress(chain.receipts[dep.TxIndex].Logs[0].Topics[1].Bytes()))
	ct.StateSet(rwBridge, "gr", "100000000000000000")
	ct.StateSet(rwBridge, "s-eth", "100000000000000000|0|0|0")
	ct.Deposit(user, 1_000_000, ledgerDb.AssetHbd)
	ps := rwUnmapUSDC(t, c, user, rwUSDC, "25000000", 0)
	mined, tx := rwMine(t, c, ps, 103, types.ReceiptStatusFailed)
	rwConfirm(t, c, mined, tx, 103, ps)
	rwExpect(t, ct, "a-"+user+"-usdc", "24750000")
	rwExpect(t, ct, "a-hive:owner-usdc", "250000")
	rwExpect(t, ct, "s-usdc", "25000000|25000000|0|0")
}

// GLAM-1: an unmap's gas_limit is the gas the TSS-signed withdrawal carries,
// and the mined withdrawal confirms.
func TestRealWasm_GLAM1_ETHWithdrawalCarriesItsGasLimit(t *testing.T) {
	c, _ := rwUSDCBridge(t)
	ethUser := rwETHUser(t, c)
	r := rwCall(c.ct, ethUser, "unmapETH", []byte(`{"to":"0x00000000000000000000000000000000000000e3","amount":"10000000000000000","asset":"eth","gas_limit":207391}`))
	if !r.Success {
		t.Fatalf("unmapETH gas_limit 207391: %s: %s", r.Err, r.ErrMsg)
	}
	ps := rwPending(t, c, 0)
	if g := rwTxGas(t, ps); g != 207_391 {
		t.Fatalf("signed gas %d, want 207391", g)
	}
	// Charged 207,391 x 4 gwei.
	rwExpect(t, c.ct, "a-"+ethUser+"-eth", new(big.Int).Sub(big.NewInt(980_000_000_000_000_000), big.NewInt(207_391*4_000_000_000)).String())
	chain, tx := rwMine(t, c, ps, 103, types.ReceiptStatusSuccessful)
	rwConfirm(t, c, chain, tx, 103, ps)
	rwExpect(t, c.ct, "n", "1")
}

// RP-7: deposits through the vault's MagiDeposit credit the named recipient:
// ETH (less the reserve share) and USDC (whose paying Transfer the plain
// ERC-20 path then refuses).
func TestRealWasm_RP7_MagiDepositCreditsTheRecipient(t *testing.T) {
	c, _ := rwUSDCBridge(t)
	r := rwCall(c.ct, "hive:anyone", "depositAddress", []byte(`{}`))
	if !r.Success || !common.IsHexAddress(r.Ret) {
		t.Fatalf("depositAddress: %v %q %s", r.Success, r.Ret, r.ErrMsg)
	}
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
	chain := &fakeChain{}
	tx := chain.addTx(t, userKey, &types.DynamicFeeTx{GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(30e9), Gas: 200_000, To: &dc, Value: big.NewInt(1e18)},
		&types.Log{Address: dc, Topics: []common.Hash{sig, common.BytesToHash(sender.Bytes()), {}}, Data: depositData(big.NewInt(1e18), "hive:bob")},
		&types.Log{Address: usdc, Topics: []common.Hash{transferSig, common.BytesToHash(sender.Bytes()), common.BytesToHash(c.vault.Bytes())}, Data: common.LeftPadBytes(big.NewInt(7_000_000).Bytes(), 32)},
		&types.Log{Address: dc, Topics: []common.Hash{sig, common.BytesToHash(sender.Bytes()), common.BytesToHash(usdc.Bytes())}, Data: depositData(big.NewInt(7_000_000), "hive:bob")},
	)
	rwHeaderAt(c.ct, 105, chain, rwBlockTime)
	_, nodes, err := receiptTrieProof(chain.rpc(t), []string{tx.Hash().Hex()}, 0)
	if err != nil {
		t.Fatal(err)
	}
	mapLog := func(li int) (bool, string) {
		p, _ := json.Marshal(map[string]any{"tx_data": map[string]any{"block_height": 105, "tx_index": 0, "log_index": li,
			"deposit_type": "contract", "receipt_proof": base64.StdEncoding.EncodeToString(joinNodes(nodes))}})
		r := rwCall(c.ct, "hive:anyone", "map", p)
		t.Logf("map contract log %d: success=%v %d RC %s", li, r.Success, r.RcUsed, r.ErrMsg)
		return r.Success, r.ErrMsg
	}
	if ok, msg := mapLog(0); !ok {
		t.Fatalf("ETH contract deposit: %s", msg)
	}
	if ok, msg := mapLog(2); !ok {
		t.Fatalf("USDC contract deposit: %s", msg)
	}
	rwExpect(t, c.ct, "a-hive:bob-eth", "990000000000000000")
	rwExpect(t, c.ct, "a-hive:bob-usdc", "7000000")
	if ok, _ := mapLog(2); ok {
		t.Fatal("the same contract deposit was credited twice")
	}
	// The paying Transfer through the plain ERC-20 path: refused.
	if r := rwCall(c.ct, "hive:anyone", "map", buildMapPayloadFromRPC(context.Background(), chain.rpc(t),
		detectedDeposit{BlockHeight: 105, TxIndex: 0, LogIndex: 1, TxHash: tx.Hash().Hex(), DepositType: "erc20", TokenAddress: rwUSDC}, 105)); r.Success {
		t.Fatal("the transfer that paid a MagiDeposit deposit was credited to its sender too")
	}
	rwExpect(t, c.ct, "a-"+rwDID(sender)+"-usdc", "")
}

func joinNodes(nodes [][]byte) []byte {
	var out []byte
	for _, n := range nodes {
		out = append(out, n...)
	}
	return out
}

// abiStringArgs is abi.encode(uint256 amount, string s).
func abiStringArgs(amount *big.Int, s string) ([]byte, error) {
	out := common.LeftPadBytes(amount.Bytes(), 32)
	out = append(out, common.LeftPadBytes(big.NewInt(64).Bytes(), 32)...)
	out = append(out, common.LeftPadBytes(big.NewInt(int64(len(s))).Bytes(), 32)...)
	padded := make([]byte, (len(s)+31)/32*32)
	copy(padded, s)
	return append(out, padded...), nil
}
