//go:build evm_devnet

// L1 DIAGNOSTIC for the roll scenarios (S10, S11).
//
// The bot's log cannot answer the question those two scenarios turn on. It
// prints every broadcast as
//
//	withdrawal TX broadcast to Ethereum txHash=0x… nonce=1 amount=… to=0x…
//
// where amount and to come from the stored withdrawal RECORD, not from the
// transaction it actually signed. A roll is executed on L1 as a 0-value
// self-transfer that burns the vault's nonce (evm_bridge_w7_roll_devnet_test.go
// asserts exactly that: "to == vault, value == 0" at the rolled nonce), so a
// roll and a payout are logged identically, and "confirmed successfully on L1"
// says nothing about whether anyone was paid.
//
// This walks the chain once and prints what the vault really sent at each
// nonce: the destination, the value, whether it carried calldata, its receipt
// status, and the destination's balance now. That distinguishes
//
//	nonce burned by a roll   -> to == vault, value 0, status 1, nobody paid
//	payout that moved ETH    -> to == recipient, value > 0, status 1, paid
//	payout the vault refused -> status 0
//
// which is what runs 1 and 3 of TestSwapSafety_PayoutReturnsAndRollsOnly left
// undecided: both reported "the recipient got 0 wei" while the bot logged the
// pre-roll payout as confirmed.
package devnet

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"
)

type l1DiagTx struct {
	From  string  `json:"from"`
	To    *string `json:"to"`
	Nonce string  `json:"nonce"`
	Value string  `json:"value"`
	Input string  `json:"input"`
	Hash  string  `json:"hash"`
}

type l1DiagBlock struct {
	Number string     `json:"number"`
	Txs    []l1DiagTx `json:"transactions"`
}

// prL1VaultTxs prints every transaction the vault sent, straight from L1.
func prL1VaultTxs(t *testing.T, x *dex3, label string) {
	b := x.b
	vault := b.vault0
	// Its own context: run 3 ended with the test's context already cancelled
	// ("stateStr phrh: read failed: ... context canceled"), and a dump that
	// cannot read is worth nothing. anvil outlives this cleanup — the devnet
	// teardown was registered first, so it runs last.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	latest, err := b.anvil.cli.BlockNumber(ctx)
	if err != nil {
		t.Logf("L1DIAG %s: block number: %v", label, err)
		return
	}
	t.Logf("L1DIAG %s: vault %s, head %d", label, vault.Hex(), latest)
	seen := 0
	for n := uint64(1); n <= latest; n++ {
		var blk l1DiagBlock
		if err := b.anvil.rpc.CallContext(ctx, &blk, "eth_getBlockByNumber", fmt.Sprintf("0x%x", n), true); err != nil {
			t.Logf("L1DIAG %s: block %d: %v", label, n, err)
			return
		}
		for _, tx := range blk.Txs {
			if !strings.EqualFold(tx.From, vault.Hex()) {
				continue
			}
			seen++
			to, kind := "nil (creation)", "contract creation"
			bal := "n/a"
			if tx.To != nil {
				to = *tx.To
				switch {
				case strings.EqualFold(to, vault.Hex()):
					kind = "SELF-TRANSFER (a roll: burns the nonce, pays nobody)"
				default:
					kind = "payment"
				}
				var h string
				if err := b.anvil.rpc.CallContext(ctx, &h, "eth_getBalance", ethcommon.HexToAddress(to), "latest"); err == nil {
					bal = h
				}
			}
			var rc struct {
				Status string `json:"status"`
			}
			st := "no receipt"
			if err := b.anvil.rpc.CallContext(ctx, &rc, "eth_getTransactionReceipt", tx.Hash); err == nil {
				st = rc.Status
			}
			val := new(big.Int)
			val.SetString(strings.TrimPrefix(tx.Value, "0x"), 16)
			t.Logf("L1DIAG %s: vault nonce %s in block %d: to %s value %s data %d bytes status %s | %s | that address holds %s now (tx %s)",
				label, tx.Nonce, n, to, val, (len(tx.Input)-2)/2, st, kind, bal, tx.Hash)
		}
	}
	if seen == 0 {
		t.Logf("L1DIAG %s: the vault sent nothing at all in blocks 1..%d", label, latest)
	}
}
