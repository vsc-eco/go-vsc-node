package main

import (
	"context"
	"encoding/hex"
	"math/big"
	"strings"
	"testing"
	"time"

	"vsc-node/lib/dids"

	"github.com/ethereum/go-ethereum/core/types"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

func actions(t *testing.T, l2 *fakeL2) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, c := range submittedCalls(t, l2) {
		out[c.Action]++
	}
	return out
}

// I-13, testnet 11-04 05:07-05:28: while the bridge was paused the bot skipped
// its whole tick. A paused bridge refuses maps and drains; the scan, which is
// read-only, goes on and its deposits stay parked, and both calls go out once
// the bridge is unpaused.
func TestI13_PausedHoldsMapsAndDrainsNotTheScan(t *testing.T) {
	lastDrainAt = time.Time{}
	l1 := newFakeL1()
	l2, gql, rpc, sub, cfg, vault := bridgeWorld(t, l1, 102)
	l2.status = func(string) string { return "UNCONFIRMED" }
	ethDeposit(t, l1.block(101), vault, big.NewInt(2e16), nil)
	l1.block(102)
	for k, v := range map[string]string{"wqh": "3", "wqt": "5", "n": "0", "np": "0"} {
		l2.set("vsc1bridge", k, v)
	}
	l2.set("vsc1verifier", "b-102", tipHeader(time.Now().Add(-5*time.Minute)))
	cp := &Checkpoint{LastScannedBlock: 100, SentWithdrawals: map[string]SentTx{}, BlockRetries: map[uint64]int{}, PendingMaps: map[string]PendingMap{}}
	cfg.Paused = true
	if err := runOnce(context.Background(), rpc, gql, sub, cp, cfg, "vsc1bridge-primary"); err != nil {
		t.Fatal(err)
	}
	if a := actions(t, l2); a["map"] != 0 || a["drainWithdrawalQueue"] != 0 {
		t.Fatalf("paused: calls %v, want no map and no drain (the bridge refuses both)", a)
	}
	if cp.LastScannedBlock != 102 || len(cp.WaitingDeposits[101]) != 1 {
		t.Fatalf("paused: scan at %d, parked %v; want the scan at 102 and the deposit parked", cp.LastScannedBlock, cp.WaitingDeposits)
	}
	cfg.Paused = false
	lastDrainAt = time.Time{}
	if err := runOnce(context.Background(), rpc, gql, sub, cp, cfg, "vsc1bridge-primary"); err != nil {
		t.Fatal(err)
	}
	if a := actions(t, l2); a["map"] != 1 || a["drainWithdrawalQueue"] != 1 {
		t.Fatalf("unpaused: calls %v, want the parked deposit's map and the drain", a)
	}
}

// While paused, a withdrawal that paid on L1 is still confirmed (the bridge
// does not gate it); one that reverted waits, since its confirm refunds it and
// a paused bridge refuses refunds (errRefundWhilePaused).
func TestI13_PausedStillConfirmsPaidWithdrawals(t *testing.T) {
	for _, c := range []struct {
		status uint64
		paused bool
		want   int
	}{{1, true, 1}, {0, true, 0}, {0, false, 1}} {
		tx0 := queuedTxNonce(t, 0)
		l1, l2, rpc, gql, cp, cfg := wdWorld(t, tx0)
		handleWithdrawals(context.Background(), rpc, gql, cp, cfg, "vsc1bridge-primary")
		if len(l1.sent) != 1 {
			t.Fatalf("broadcasts %d, want 1", len(l1.sent))
		}
		b, _ := hex.DecodeString(l1.sent[0][2:])
		var tx types.Transaction
		if err := tx.UnmarshalBinary(b); err != nil {
			t.Fatal(err)
		}
		blk := l1.block(101)
		blk.txs, blk.receipts = append(blk.txs, &tx), append(blk.receipts, &types.Receipt{Type: types.DynamicFeeTxType, Status: c.status, CumulativeGasUsed: 21000, TxHash: tx.Hash()})
		l2.set("vsc1bridge", "zkverifier", "vsc1verifier")
		l2.set("vsc1verifier", "h", "101")
		l2.status = func(string) string { return "UNCONFIRMED" }
		botKey, _ := ethCrypto.GenerateKey()
		sub := &l2Submitter{ethKey: botKey, did: dids.NewEthDID(ethCrypto.PubkeyToAddress(botKey.PublicKey).Hex()), gql: gql, cfg: cfg}
		cfg.Paused = c.paused
		handleConfirmations(context.Background(), rpc, gql, sub, cp, cfg)
		got := 0
		for _, call := range submittedCalls(t, l2) {
			if call.Action == "confirmSpend" && strings.Contains(call.Payload, `"intent_nonce":0`) {
				got++
			}
		}
		if got != c.want {
			t.Fatalf("receipt status %d, paused %v: %d confirmSpend, want %d", c.status, c.paused, got, c.want)
		}
	}
}
