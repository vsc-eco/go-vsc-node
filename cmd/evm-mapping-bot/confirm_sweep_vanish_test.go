package main

import (
	"context"
	"encoding/hex"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vsc-node/lib/btcvault"
	"vsc-node/lib/dids"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

type sweepConfirmWorld struct {
	l2    *fakeL2
	gql   *vscGraphQL
	cp    *Checkpoint
	drive func(*vscGraphQL)
}

// newSweepConfirmWorld is a mined, final sweep 0-7 with its header stored. It
// returns after the drive that submits the confirmSweep.
func newSweepConfirmWorld(t *testing.T, pool *fakePool) *sweepConfirmWorld {
	t.Helper()
	succ := common.HexToAddress(rotAddr(2))
	unsigned := rotUnsigned(succ, big.NewInt(5), nil)
	sh, _ := computeSighash(unsigned)
	view, _ := decodeVaultView("vsc1bridge", hex.EncodeToString(rotRegistry(rotEntry{0, 1, btcvault.VaultStatusDraining}, rotEntry{1, 2, btcvault.VaultStatusActive})))
	rec := sweepRecord{Gen: 0, Nonce: 7, Asset: "eth", BuiltL1: 10, Versions: []sweepVersion{{Unsigned: unsigned}}}
	l1, l2 := newFakeL1(), newFakeL2()
	l2.pool = pool
	l2.sigs[sh] = tssSigWith(t, rotKey(1), unsigned)
	l2.set("vsc1bridge", "zkverifier", "vsc1verifier")
	l2.set("vsc1verifier", "h", "100")
	l2.sim = func(string) map[string]any {
		return map[string]any{"success": true, "ret": "ok", "rc_used": int64(900)}
	}
	rpc, gql := l1.rpc(t), l2.gql(t)
	botKey, _ := ethCrypto.GenerateKey()
	sub := &l2Submitter{ethKey: botKey, did: dids.NewEthDID(ethCrypto.PubkeyToAddress(botKey.PublicKey).Hex()), gql: gql,
		cfg: EVMBotConfig{ContractID: "vsc1bridge", NetID: "vsc-devnet", RcLimit: 10000}}
	cfg := EVMBotConfig{ContractID: "vsc1bridge"}
	cp := &Checkpoint{SentSweeps: map[string]SentSweep{}}
	w := &sweepConfirmWorld{l2: l2, gql: gql, cp: cp}
	w.drive = func(g *vscGraphQL) { driveSweep(context.Background(), rpc, g, sub, cp, cfg, view, rec, false) }
	w.drive(gql) // signed + broadcast
	b, _ := hex.DecodeString(strings.TrimPrefix(l1.sent[0], "0x"))
	var tx types.Transaction
	if err := tx.UnmarshalBinary(b); err != nil {
		t.Fatal(err)
	}
	c := l1.block(50)
	c.txs, c.receipts = append(c.txs, &tx), append(c.receipts, &types.Receipt{Type: 2, Status: 1, TxHash: tx.Hash()})
	w.drive(gql) // mined, final, header stored: confirmSweep
	first := cp.SentSweeps["0-7"]
	calls := submittedCalls(t, l2)
	if len(calls) != 1 || calls[0].Action != "confirmSweep" || first.ConfirmTxId == "" || first.ConfirmSubmittedAt == 0 {
		t.Fatalf("calls %+v, entry %+v; want one confirmSweep submitted", calls, first)
	}
	return w
}

func vanishGraceZero(t *testing.T) {
	t.Helper()
	old := confirmVanishGrace
	confirmVanishGrace = 0
	t.Cleanup(func() { confirmVanishGrace = old })
	time.Sleep(1100 * time.Millisecond) // ConfirmSubmittedAt has second resolution
}

// FS2 11-03: B-R1b took a confirmSweep's nonce; it was never sent again, so the
// sweep stayed open, the books at the old generation, and the withdrawal queued
// in the migration was never issued. Unknown long after its submission, it is
// re-sent, as a confirmSpend is (B-R1).
func TestA01Bot_AVanishedConfirmSweepIsResent(t *testing.T) {
	w := newSweepConfirmWorld(t, &fakePool{})
	l2 := w.l2
	first := w.cp.SentSweeps["0-7"]
	// Another call the node saw first takes the nonce; the block deletes the confirmSweep.
	l2.mu.Lock()
	auth := l2.pool.txs[0].auth
	l2.pool.txs = append([]*fakePoolTx{{id: "competitor", status: "UNCONFIRMED", auth: auth, nonce: 0}}, l2.pool.txs...)
	l2.pool.produce()
	l2.mu.Unlock()
	if s := status(l2, first.ConfirmTxId); s != "gone" {
		t.Fatalf("confirmSweep %s after the competing block, want deleted", s)
	}

	w.drive(w.gql) // unknown, inside the grace: wait
	if n := len(submittedCalls(t, l2)); n != 1 || w.cp.SentSweeps["0-7"].ConfirmTxId != first.ConfirmTxId {
		t.Fatalf("inside the grace: %d calls, entry %+v", n, w.cp.SentSweeps["0-7"])
	}
	vanishGraceZero(t)
	w.drive(w.gql) // gone: cleared
	w.drive(w.gql) // sent again
	again := w.cp.SentSweeps["0-7"]
	calls := submittedCalls(t, l2)
	if len(calls) != 2 || calls[1].Action != "confirmSweep" || again.ConfirmTxId == "" || again.ConfirmTxId == first.ConfirmTxId || again.ConfirmSubmittedAt == 0 {
		t.Fatalf("past the grace: calls %+v, entry %+v; want the confirmSweep re-sent", calls, again)
	}
	// It lands: nothing more is sent.
	l2.mu.Lock()
	l2.pool.produce()
	l2.mu.Unlock()
	w.drive(w.gql)
	w.drive(w.gql)
	if n := len(submittedCalls(t, l2)); n != 2 {
		t.Fatalf("%d calls after the re-sent confirmSweep landed, want 2", n)
	}
}

// A checkpoint from before ConfirmSubmittedAt: the clock starts at the first
// "not found", so a call just sent by the old bot is not re-sent at once.
func TestA01Bot_ConfirmSweepFromAnOldCheckpointGetsItsGrace(t *testing.T) {
	w := newSweepConfirmWorld(t, nil)
	w.l2.status = func(string) string { return "-" }
	ss := w.cp.SentSweeps["0-7"]
	ss.ConfirmSubmittedAt = 0
	w.cp.SentSweeps["0-7"] = ss
	w.drive(w.gql)
	got := w.cp.SentSweeps["0-7"]
	if got.ConfirmTxId != ss.ConfirmTxId || got.ConfirmSubmittedAt == 0 || len(submittedCalls(t, w.l2)) != 1 {
		t.Fatalf("entry %+v: want the same call, its clock started, nothing re-sent", got)
	}
}

// Only "not found" means gone. Pending, or a read that failed, is still waited
// on (the call may yet land; a second one would fail and cost RC). FAILED is
// retried after the vault-op interval.
func TestA01Bot_OnlyAnUnknownConfirmSweepIsResent(t *testing.T) {
	vanishGraceZero(t)
	w := newSweepConfirmWorld(t, nil)
	first := w.cp.SentSweeps["0-7"]
	time.Sleep(1100 * time.Millisecond)
	for _, s := range []string{"UNCONFIRMED", "INCLUDED", "CONFIRMED"} {
		s := s
		w.l2.status = func(string) string { return s }
		w.drive(w.gql)
		if got := w.cp.SentSweeps["0-7"]; got.ConfirmTxId != first.ConfirmTxId || len(submittedCalls(t, w.l2)) != 1 {
			t.Fatalf("%s: entry %+v, %d calls", s, got, len(submittedCalls(t, w.l2)))
		}
	}
	oldPauses := callRetryPauses
	callRetryPauses = nil
	t.Cleanup(func() { callRetryPauses = oldPauses })
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	t.Cleanup(down.Close)
	w.drive(&vscGraphQL{urls: []string{down.URL}, client: http.DefaultClient})
	if got := w.cp.SentSweeps["0-7"]; got.ConfirmTxId != first.ConfirmTxId {
		t.Fatalf("an unreadable status cleared the confirmSweep: %+v", got)
	}

	w.l2.status = func(string) string { return "FAILED" }
	w.drive(w.gql)
	got := w.cp.SentSweeps["0-7"]
	if got.ConfirmTxId != "" || got.NextTryAt <= time.Now().Unix() {
		t.Fatalf("FAILED: entry %+v, want cleared with a later retry", got)
	}
	w.drive(w.gql) // the interval has not passed
	if n := len(submittedCalls(t, w.l2)); n != 1 {
		t.Fatalf("%d calls before the retry time", n)
	}
}

// A node lagging past the grace can report a confirmSweep that did land as
// unknown. The second one costs RC and nothing else: the dry run refuses it
// when the node has caught up (realwasm_a01_test.go: on the wasm the sweep has
// no open record and nothing changes), and the record is gone from msr, so
// driveVaultRotation stops driving it.
func TestA01Bot_AConfirmSweepThatLandedIsNotSentAgain(t *testing.T) {
	vanishGraceZero(t)
	w := newSweepConfirmWorld(t, nil)
	time.Sleep(1100 * time.Millisecond)
	w.l2.status = func(string) string { return "-" }
	w.l2.sim = func(string) map[string]any {
		return map[string]any{"success": false, "err": "ErrIntent", "err_msg": "no open sweep 0-7", "rc_used": int64(120)}
	}
	w.drive(w.gql) // unknown past the grace: cleared
	w.drive(w.gql) // rebuilt; the dry run refuses it
	got := w.cp.SentSweeps["0-7"]
	if n := len(submittedCalls(t, w.l2)); n != 1 || got.ConfirmTxId != "" || got.NextTryAt <= time.Now().Unix() {
		t.Fatalf("%d calls, entry %+v; want no second call and a later retry", n, got)
	}
}
