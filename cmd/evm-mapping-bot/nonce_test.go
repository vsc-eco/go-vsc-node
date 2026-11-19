package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"vsc-node/lib/dids"
	"vsc-node/modules/common"
	transactionpool "vsc-node/modules/transaction-pool"

	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

// poolWorld is a bot submitter against a fake node whose pool and account
// nonce behave as the real node's (fakePool).
func poolWorld(t *testing.T) (*fakeL2, *l2Submitter) {
	t.Helper()
	l2 := newFakeL2()
	l2.pool = &fakePool{}
	botKey, _ := ethCrypto.GenerateKey()
	cfg := EVMBotConfig{ContractID: "vsc1bridge", NetID: "vsc-devnet", RcLimit: 10000}
	return l2, &l2Submitter{ethKey: botKey, did: dids.NewEthDID(ethCrypto.PubkeyToAddress(botKey.PublicKey).Hex()), gql: l2.gql(t), cfg: cfg}
}

func submittedNonces(t *testing.T, l2 *fakeL2) []uint64 {
	t.Helper()
	l2.mu.Lock()
	defer l2.mu.Unlock()
	var out []uint64
	for _, b := range l2.submitted {
		var shell transactionpool.VSCTransactionShell
		if err := common.DecodeCbor(b, &shell); err != nil {
			t.Fatal(err)
		}
		out = append(out, shell.Headers.Nonce)
	}
	return out
}

func status(l2 *fakeL2, id string) string {
	l2.mu.Lock()
	defer l2.mu.Unlock()
	if tx := l2.pool.byID(id); tx != nil {
		return tx.status
	}
	return "gone"
}

// B-R1: the node's account nonce counts only included transactions, so two
// calls before the next block (two deposits in one tick) both took it; the
// block included one and the node deleted the other. They now take n, n+1.
func TestBR1_CallsBeforeABlockTakeConsecutiveNonces(t *testing.T) {
	l2, sub := poolWorld(t)
	ctx := context.Background()
	a, err := sub.callWithRetry(ctx, "vsc1bridge", "map", json.RawMessage(`{"d":1}`), 3)
	if err != nil {
		t.Fatal(err)
	}
	b, err := sub.callWithRetry(ctx, "vsc1bridge", "map", json.RawMessage(`{"d":2}`), 3)
	if err != nil {
		t.Fatal(err)
	}
	if got := submittedNonces(t, l2); len(got) != 2 || got[0] != 0 || got[1] != 1 {
		t.Fatalf("nonces %v, want [0 1]", got)
	}
	l2.pool.produce()
	if sa, sb := status(l2, a), status(l2, b); sa != "CONFIRMED" || sb != "CONFIRMED" {
		t.Fatalf("after a block: first %s, second %s; want both CONFIRMED", sa, sb)
	}

	// Next tick, after the block: the account nonce has caught up.
	c, err := sub.callWithRetry(ctx, "vsc1bridge", "map", json.RawMessage(`{"d":3}`), 3)
	if err != nil {
		t.Fatal(err)
	}
	if got := submittedNonces(t, l2); got[2] != 2 {
		t.Fatalf("nonces %v, want the third at 2", got)
	}
	l2.pool.produce()
	if s := status(l2, c); s != "CONFIRMED" {
		t.Fatalf("third: %s", s)
	}
}

// B-R1: a bot restarted with its own transactions still in the pool starts
// after them; from the account nonce it would compete with them, and one of
// each pair would be deleted.
func TestBR1_ARestartedBotStartsAfterItsPendingTransactions(t *testing.T) {
	l2, sub := poolWorld(t)
	ctx := context.Background()
	before, err := sub.callWithRetry(ctx, "vsc1bridge", "map", json.RawMessage(`{"d":1}`), 3)
	if err != nil {
		t.Fatal(err)
	}
	restarted := &l2Submitter{ethKey: sub.ethKey, did: sub.did, gql: sub.gql, cfg: sub.cfg}
	after, err := restarted.callWithRetry(ctx, "vsc1bridge", "map", json.RawMessage(`{"d":2}`), 3)
	if err != nil {
		t.Fatal(err)
	}
	if got := submittedNonces(t, l2); len(got) != 2 || got[1] != 1 {
		t.Fatalf("nonces %v, want the restarted bot's call at 1", got)
	}
	l2.pool.produce()
	if s1, s2 := status(l2, before), status(l2, after); s1 != "CONFIRMED" || s2 != "CONFIRMED" {
		t.Fatalf("before the restart %s, after %s; want both CONFIRMED", s1, s2)
	}
}

// B-R1: when the node lacks the bot's previous transaction (it never arrived,
// or went to a node that is gone), the bot keeps its nonce through
// nonceHoleGrace, which covers gossip, and then refills from the account nonce.
func TestBR1_AHoleIsRefilledAfterTheGrace(t *testing.T) {
	l2, sub := poolWorld(t)
	ctx := context.Background()
	if _, err := sub.callWithRetry(ctx, "vsc1bridge", "map", json.RawMessage(`{"d":1}`), 1); err != nil {
		t.Fatal(err)
	}
	l2.mu.Lock()
	l2.pool.txs = nil // the node lost nonce 0
	l2.mu.Unlock()

	if _, err := sub.callWithRetry(ctx, "vsc1bridge", "map", json.RawMessage(`{"d":2}`), 1); err == nil || !strings.Contains(err.Error(), "missing previous nonce") {
		t.Fatalf("inside the grace: err %v, want the node's missing-previous-nonce refusal", err)
	}
	old := nonceHoleGrace
	nonceHoleGrace = 0
	t.Cleanup(func() { nonceHoleGrace = old })
	time.Sleep(time.Millisecond)
	if _, err := sub.callWithRetry(ctx, "vsc1bridge", "map", json.RawMessage(`{"d":2}`), 1); err == nil {
		t.Fatal("past the grace the refusal resets the nonce; the call itself still fails")
	}
	id, err := sub.callWithRetry(ctx, "vsc1bridge", "map", json.RawMessage(`{"d":2}`), 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := submittedNonces(t, l2); got[len(got)-1] != 0 {
		t.Fatalf("nonces %v, want the refill at 0", got)
	}
	l2.pool.produce()
	if s := status(l2, id); s != "CONFIRMED" {
		t.Fatalf("refilled call: %s", s)
	}
}

// B-R7: a nonce-consumed refusal returned no id, so nothing tracked the call.
// Here an attempt that saw an error after the node took the call runs again,
// with an account nonce read before the block included it: the node refuses
// "nonce too low" and the bot now returns the id the node gave those bytes.
func TestBR7_ConsumedNonceReturnsTheTransactionsID(t *testing.T) {
	l2, sub := poolWorld(t)
	ctx := context.Background()
	first, err := sub.callWithRetry(ctx, "vsc1bridge", "map", json.RawMessage(`{"d":1}`), 3)
	if err != nil {
		t.Fatal(err)
	}
	l2.mu.Lock()
	l2.pool.produce()
	stale := uint64(0)
	l2.pool.staleRead = &stale
	l2.mu.Unlock()
	sub.haveNext = false // the bot did not see its first attempt succeed

	got, err := sub.callWithRetry(ctx, "vsc1bridge", "map", json.RawMessage(`{"d":1}`), 3)
	if err != nil {
		t.Fatal(err)
	}
	if got != first || status(l2, got) != "CONFIRMED" {
		t.Fatalf("id %q (status %s), want the landed call's id %s", got, status(l2, got), first)
	}
}

// B-R1: a confirmSpend the node deleted (another transaction took its nonce)
// was waited on for ever: the withdrawal was never closed. Unknown long after
// its submission, it is re-sent.
func TestBR1_AVanishedConfirmSpendIsResent(t *testing.T) {
	l2, rpc, gql, cp, cfg, sub := confirmWorld(t)
	l2.pool = &fakePool{}
	tick := func() { handleConfirmations(context.Background(), rpc, gql, sub, cp, cfg) }
	tick()
	first := cp.SentWithdrawals["0"]
	if len(l2.submitted) != 1 || first.ConfirmTxId == "" || first.ConfirmSubmittedAt == 0 {
		t.Fatalf("first tick: %d submissions, entry %+v", len(l2.submitted), first)
	}
	// A transaction the node saw first takes nonce 0; the block deletes the
	// confirmSpend.
	l2.mu.Lock()
	l2.pool.txs = append([]*fakePoolTx{{id: "competitor", status: "UNCONFIRMED", auth: sub.did.String(), nonce: 0}}, l2.pool.txs...)
	l2.pool.produce()
	l2.mu.Unlock()
	if s := status(l2, first.ConfirmTxId); s != "gone" {
		t.Fatalf("confirmSpend %s after the competing block, want deleted", s)
	}

	tick() // unknown, inside the grace: wait
	if len(l2.submitted) != 1 || cp.SentWithdrawals["0"].ConfirmTxId != first.ConfirmTxId {
		t.Fatalf("inside the grace: %d submissions, entry %+v", len(l2.submitted), cp.SentWithdrawals["0"])
	}
	old := confirmVanishGrace
	confirmVanishGrace = 0
	t.Cleanup(func() { confirmVanishGrace = old })
	time.Sleep(1100 * time.Millisecond) // ConfirmSubmittedAt has second resolution
	tick()                              // gone: cleared
	tick()                              // re-sent, at the next nonce
	again := cp.SentWithdrawals["0"]
	if len(l2.submitted) != 2 || again.ConfirmTxId == "" || again.ConfirmTxId == first.ConfirmTxId {
		t.Fatalf("past the grace: %d submissions, entry %+v; want the confirmSpend re-sent", len(l2.submitted), again)
	}
	l2.mu.Lock()
	l2.pool.produce()
	l2.mu.Unlock()
	tick()
	if _, ok := cp.SentWithdrawals["0"]; ok {
		t.Fatalf("the re-sent confirmSpend (%s) did not close the withdrawal", status(l2, again.ConfirmTxId))
	}
}
