package main

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"

	"vsc-node/lib/dids"

	"github.com/ethereum/go-ethereum/core/types"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

// I-21, testnet 11-04 06:01-08:47: three withdrawals mined in three blocks,
// none of their headers stored. Only the head's header was asked for, so each
// later one was first wanted after the one before it confirmed: one confirm
// per proof cycle while the queue grew to 16. Now one pass asks for all three
// headers; once they are stored, the three confirms go out in nonce order.
func TestI21_EveryFinalWithdrawalsHeaderIsAskedForAtOnce(t *testing.T) {
	tx0 := queuedTxNonce(t, 0)
	l1, l2, rpc, gql, cp, cfg := wdWorld(t, tx0)
	l2.set("vsc1bridge", "np", "3")
	for n := uint64(1); n <= 2; n++ {
		txn := queuedTxNonce(t, n)
		l2.set("vsc1bridge", "d-"+string(rune('0'+n)), spendJSONNonce(txn, n))
		sh, _ := computeSighash(txn)
		l2.sigs[sh] = tssSig(t, txn)
	}
	handleWithdrawals(context.Background(), rpc, gql, cp, cfg, "vsc1bridge-primary")
	if len(l1.sent) != 3 {
		t.Fatalf("broadcasts %d, want nonces 0-2", len(l1.sent))
	}
	// nonce n mines in block 101+n; all final; the verifier stores no header yet
	for i, s := range l1.sent {
		b, _ := hex.DecodeString(s[2:])
		var tx types.Transaction
		if err := tx.UnmarshalBinary(b); err != nil {
			t.Fatal(err)
		}
		c := l1.block(101 + uint64(i))
		c.txs, c.receipts = append(c.txs, &tx), append(c.receipts, &types.Receipt{Type: types.DynamicFeeTxType, Status: 1, CumulativeGasUsed: 21000, TxHash: tx.Hash()})
	}
	l2.onDemand = true
	l2.set("vsc1bridge", "zkverifier", "vsc1verifier")
	l2.set("vsc1verifier", "h", "103")
	l2.status = func(string) string { return "UNCONFIRMED" }
	botKey, _ := ethCrypto.GenerateKey()
	sub := &l2Submitter{ethKey: botKey, did: dids.NewEthDID(ethCrypto.PubkeyToAddress(botKey.PublicKey).Hex()), gql: gql, cfg: cfg}

	// block 103 is not final yet: nonce 2 asks for nothing (its block could move)
	l1.mu.Lock()
	l1.finalized = 102
	l1.mu.Unlock()
	handleConfirmations(context.Background(), rpc, gql, sub, cp, cfg)
	if _, ok := cp.HeaderNeeds[103]; ok || cp.SentWithdrawals["2"].MinedBlock != 0 {
		t.Fatalf("header needs %v, nonce 2 mined block %d: a block not yet final was asked for", cp.HeaderNeeds, cp.SentWithdrawals["2"].MinedBlock)
	}
	l1.mu.Lock()
	l1.finalized = 103
	l1.mu.Unlock()
	handleConfirmations(context.Background(), rpc, gql, sub, cp, cfg)
	for _, b := range []uint64{101, 102, 103} {
		if _, ok := cp.HeaderNeeds[b]; !ok {
			t.Fatalf("header needs %v: block %d's withdrawal waits a whole proof cycle before its header is asked for", cp.HeaderNeeds, b)
		}
	}
	if calls := submittedCalls(t, l2); len(calls) != 0 {
		t.Fatalf("calls %+v before any header is stored", calls)
	}
	if cp.SentWithdrawals["2"].MinedBlock != 103 {
		t.Fatalf("nonce 2's mined block %d not kept, want 103", cp.SentWithdrawals["2"].MinedBlock)
	}

	// one proof run stores the three headers: the three confirms, in order
	for _, b := range []string{"b-101", "b-102", "b-103"} {
		l2.set("vsc1verifier", b, strings.Repeat("h", 137))
	}
	handleConfirmations(context.Background(), rpc, gql, sub, cp, cfg)
	calls := submittedCalls(t, l2)
	if len(calls) != 3 {
		t.Fatalf("%d calls after the headers were stored, want 3 confirmSpends: %+v", len(calls), calls)
	}
	for i, c := range calls {
		if c.Action != "confirmSpend" || !strings.Contains(c.Payload, `"intent_nonce":`+string(rune('0'+i))) {
			t.Fatalf("call %d is %s %.80s, want confirmSpend for nonce %d", i, c.Action, c.Payload, i)
		}
	}
}

// A later withdrawal that has not mined asks for nothing.
func TestI21_UnminedWithdrawalsAskForNothing(t *testing.T) {
	l2, rpc, gql, cp, cfg, sub := confirmWorld(t)
	l2.set("vsc1bridge", "n", "0")
	head := cp.SentWithdrawals["0"]
	head.TxHash = "0x" + strings.Repeat("ee", 32) // the head has not mined
	cp.SentWithdrawals["0"] = head
	cp.SentWithdrawals["1"] = SentTx{TxHash: "0x" + strings.Repeat("dd", 32), Nonce: 1, SentAt: 1} // nor has nonce 1
	handleConfirmations(context.Background(), rpc, gql, sub, cp, cfg)
	if len(cp.HeaderNeeds) != 0 {
		t.Fatalf("header needs %v for withdrawals that have not mined", cp.HeaderNeeds)
	}
}
