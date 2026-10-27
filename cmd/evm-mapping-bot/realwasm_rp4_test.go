//go:build evm_realwasm

package main

import (
	"context"
	"math/big"
	"strings"
	"testing"

	ledgerDb "vsc-node/modules/db/vsc/ledger"

	"github.com/ethereum/go-ethereum/core/types"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

// RP-4: the 0.05 ETH reserve floor (MinGasReserve) gates the ETH withdrawal
// path too, although an ETH withdrawal is paid by its user and never touches
// the reserve. An ERC-20 withdrawal pays its gas out of the reserve, so one
// USDC withdrawal on a reserve at the floor leaves it below, and from then on
// an ETH withdrawal by a user who holds the ETH is refused. Topping the reserve
// back up by that gas lets the same ETH withdrawal through: the floor is the
// only thing in its way.
func TestRealWasm_RP4_ERC20GasDrainsTheFloorAndLocksETHWithdrawals(t *testing.T) {
	c, usdcUser := rwUSDCBridge(t)

	ethUser := rwETHUser(t, c)

	// setReserve moves the reserve to wei, booking the same delta on ETH
	// custody (s-eth's first field, Active) so custody stays consistent.
	setReserve := func(wei *big.Int) {
		t.Helper()
		old, _ := new(big.Int).SetString(c.ct.StateGet(rwBridge, "gr"), 10)
		f := strings.Split(c.ct.StateGet(rwBridge, "s-eth"), "|")
		active, ok := new(big.Int).SetString(f[0], 10)
		if old == nil || !ok {
			t.Fatalf("gr=%q s-eth=%q", c.ct.StateGet(rwBridge, "gr"), c.ct.StateGet(rwBridge, "s-eth"))
		}
		f[0] = active.Add(active, new(big.Int).Sub(wei, old)).String()
		c.ct.StateSet(rwBridge, "gr", wei.String())
		c.ct.StateSet(rwBridge, "s-eth", strings.Join(f, "|"))
	}
	floor := big.NewInt(50_000_000_000_000_000) // MinGasReserve, constants.go:166
	setReserve(floor)

	// One USDC withdrawal on a reserve exactly at the floor is allowed and
	// pays its gas from it. It is confirmed before the ETH withdrawal, since
	// only one withdrawal may be pending (RP-11).
	ps := rwUnmapUSDC(t, c, usdcUser, "0x00000000000000000000000000000000000000e2", "1000000", 0)
	after, _ := new(big.Int).SetString(c.ct.StateGet(rwBridge, "gr"), 10)
	gas := new(big.Int).Sub(floor, after)
	if gas.Sign() <= 0 || after.Cmp(floor) >= 0 {
		t.Fatalf("reserve %s after the USDC withdrawal, want below the floor %s", after, floor)
	}
	chain2, tx2 := rwMine(t, c, ps, 103, types.ReceiptStatusSuccessful)
	rwConfirm(t, c, chain2, tx2, 103, ps)
	rwExpect(t, c.ct, "n", "1")
	rwExpect(t, c.ct, "gr", after.String())
	t.Logf("one USDC withdrawal took %s wei of gas from the reserve: %s -> %s, and stays taken after confirmSpend", gas, floor, after)

	// The ETH user, holding 0.99 ETH and paying their own fee, is refused.
	unmapETH := []byte(`{"to":"0x00000000000000000000000000000000000000e3","amount":"10000000000000000","asset":"eth"}`)
	r := rwCall(c.ct, ethUser, "unmapETH", unmapETH)
	if r.Success || !strings.Contains(r.ErrMsg, "insufficient gas reserve") {
		t.Fatalf("unmapETH with the reserve below the floor: success=%v %s: %s; want the floor refusal", r.Success, r.Err, r.ErrMsg)
	}
	rwExpect(t, c.ct, "a-"+ethUser+"-eth", "990000000000000000")
	t.Logf("RP-4: a self-funded 0.01 ETH withdrawal is refused: %s", r.ErrMsg)

	// Control: put back exactly the gas the USDC withdrawal took; the same call goes through.
	setReserve(floor)
	if r := rwCall(c.ct, ethUser, "unmapETH", unmapETH); !r.Success {
		t.Fatalf("unmapETH with the reserve back at the floor: %s: %s", r.Err, r.ErrMsg)
	}
	if got := c.ct.StateGet(rwBridge, "gr"); got != floor.String() {
		t.Fatalf("the ETH withdrawal moved the reserve %s -> %s; it is paid by its user", floor, got)
	}
}

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
	c.ct.StateSet(rwBridge, "rl-"+rwRelayer, "1")
	dep := detectedDeposit{BlockHeight: 102, TxIndex: 0, LogIndex: -1, TxHash: tx.Hash().Hex(), DepositType: "eth"}
	if r := rwCall(c.ct, rwRelayer, "map", buildMapPayloadFromRPC(context.Background(), chain.rpc(t), dep, dep.BlockHeight)); !r.Success {
		t.Fatalf("map ETH deposit: %s: %s", r.Err, r.ErrMsg)
	}
	ethUser := rwDID(ethCrypto.PubkeyToAddress(ethKey.PublicKey))
	rwExpect(t, c.ct, "a-"+ethUser+"-eth", "990000000000000000")
	c.ct.Deposit(ethUser, 1_000_000, ledgerDb.AssetHbd) // RC for an EVM identity
	return ethUser
}

// RP-11: one pending withdrawal blocks every user's next withdrawal. While a
// USDC withdrawal is queued and unconfirmed, a different user's ETH
// withdrawal (funded, reserve well above the floor) is refused; once the
// USDC withdrawal is confirmed, the same call goes through.
func TestRealWasm_RP11_OnePendingWithdrawalBlocksEveryUser(t *testing.T) {
	c, usdcUser := rwUSDCBridge(t)
	ethUser := rwETHUser(t, c)
	ps := rwUnmapUSDC(t, c, usdcUser, "0x00000000000000000000000000000000000000e2", "1000000", 0)
	unmapETH := []byte(`{"to":"0x00000000000000000000000000000000000000e3","amount":"10000000000000000","asset":"eth"}`)
	r := rwCall(c.ct, ethUser, "unmapETH", unmapETH)
	if r.Success || !strings.Contains(r.ErrMsg, "withdrawal pending") {
		t.Fatalf("a second user's unmapETH while another withdrawal is pending: success=%v %s: %s; want the pending refusal", r.Success, r.Err, r.ErrMsg)
	}
	t.Logf("RP-11: reserve %s (floor 0.05 ETH), user funded; refused only because another user's withdrawal is pending: %s",
		c.ct.StateGet(rwBridge, "gr"), r.ErrMsg)
	chain, tx := rwMine(t, c, ps, 103, types.ReceiptStatusSuccessful)
	rwConfirm(t, c, chain, tx, 103, ps)
	if r := rwCall(c.ct, ethUser, "unmapETH", unmapETH); !r.Success {
		t.Fatalf("unmapETH after the USDC withdrawal was confirmed: %s: %s", r.Err, r.ErrMsg)
	}
}
