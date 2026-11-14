package main

import (
	"context"
	"encoding/hex"
	"math/big"
	"strings"
	"testing"

	"vsc-node/lib/btcvault"
	"vsc-node/lib/dids"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

// SWEEP-RESTART-1 (testnet 11-14, F3): the bot was SIGKILLed right after
// broadcasting sweep 5-7 and before its checkpoint was written. The sweep mined;
// the restarted bot had no record of it, re-sent it every cycle ("nonce too low")
// and never confirmed it, so the ETH books stayed on the old generation and the
// queued withdrawals were never issued.

type sweepRestartWorld struct {
	l1           *fakeL1
	l2           *fakeL2
	drive        func(cp *Checkpoint)
	driveRedrive func(cp *Checkpoint) // driveSweep with re-drives allowed
}

// newSweepRestartWorld is an open sweep 0-7 (one signed version) whose header
// store and dry runs answer as in newSweepConfirmWorld.
func newSweepRestartWorld(t *testing.T) *sweepRestartWorld {
	t.Helper()
	succ := common.HexToAddress(rotAddr(2))
	unsigned := rotUnsigned(succ, big.NewInt(5), nil)
	sh, _ := computeSighash(unsigned)
	view, _ := decodeVaultView("vsc1bridge", hex.EncodeToString(rotRegistry(rotEntry{0, 1, btcvault.VaultStatusDraining}, rotEntry{1, 2, btcvault.VaultStatusActive})))
	rec := sweepRecord{Gen: 0, Nonce: 7, Asset: "eth", BuiltL1: 10, Versions: []sweepVersion{{Unsigned: unsigned}}}
	l1, l2 := newFakeL1(), newFakeL2()
	l2.pool = &fakePool{}
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
	return &sweepRestartWorld{l1: l1, l2: l2, drive: func(cp *Checkpoint) {
		driveSweep(context.Background(), rpc, gql, sub, cp, cfg, view, rec, false)
	}, driveRedrive: func(cp *Checkpoint) {
		driveSweep(context.Background(), rpc, gql, sub, cp, cfg, view, rec, true)
	}}
}

// mineFirstSent puts the first broadcast tx in block 50 (final) and returns its hash.
func (w *sweepRestartWorld) mineFirstSent(t *testing.T) string {
	t.Helper()
	w.l1.mu.Lock()
	raw := w.l1.sent[0]
	w.l1.mu.Unlock()
	b, _ := hex.DecodeString(strings.TrimPrefix(raw, "0x"))
	var tx types.Transaction
	if err := tx.UnmarshalBinary(b); err != nil {
		t.Fatal(err)
	}
	c := w.l1.block(50)
	c.txs, c.receipts = append(c.txs, &tx), append(c.receipts, &types.Receipt{Type: 2, Status: 1, TxHash: tx.Hash()})
	return strings.ToLower(tx.Hash().Hex())
}

func (w *sweepRestartWorld) sentCount() int {
	w.l1.mu.Lock()
	defer w.l1.mu.Unlock()
	return len(w.l1.sent)
}

func confirmSweeps(t *testing.T, l2 *fakeL2) int {
	t.Helper()
	n := 0
	for _, c := range submittedCalls(t, l2) {
		if c.Action == "confirmSweep" {
			n++
		}
	}
	return n
}

func TestSWEEPRESTART1_MinedSweepIsConfirmedAfterARestartWithAnEmptyCheckpoint(t *testing.T) {
	w := newSweepRestartWorld(t)
	w.drive(&Checkpoint{SentSweeps: map[string]SentSweep{}}) // the bot that dies: broadcasts, checkpoint lost
	if w.sentCount() != 1 {
		t.Fatalf("first bot broadcast %d txs, want 1", w.sentCount())
	}
	hash := w.mineFirstSent(t)
	w.l1.refuseMined = true // a real node refuses a mined tx again: "nonce too low"

	cp := &Checkpoint{SentSweeps: map[string]SentSweep{}} // restarted onto an empty checkpoint
	w.drive(cp)
	got := cp.SentSweeps["0-7"]
	if len(got.TxHashes) != 1 || got.TxHashes[0] != hash {
		t.Fatalf("restarted bot recorded %v, want the mined %s", got.TxHashes, hash)
	}
	if n := confirmSweeps(t, w.l2); n != 1 || got.ConfirmTxId == "" {
		t.Fatalf("restarted bot submitted %d confirmSweep (entry %+v), want 1", n, got)
	}
	if w.sentCount() != 1 {
		t.Fatalf("restarted bot re-broadcast the mined sweep (%d broadcasts), want none", w.sentCount()-1)
	}
	w.drive(cp) // settled once: nothing more is sent
	if n := confirmSweeps(t, w.l2); n != 1 || w.sentCount() != 1 {
		t.Fatalf("after the confirm: %d confirmSweeps, %d broadcasts; want 1 and 1", n, w.sentCount())
	}
}

// MED-50 for sweeps: a receipt the RPC cannot back with the transaction object for
// exactly that hash is not trusted (a hostile RPC could otherwise make the bot
// record a phantom sweep and stop broadcasting the real one).
func TestSWEEPRESTART1_AReceiptWithoutItsTransactionIsNotRecorded(t *testing.T) {
	for name, reported := range map[string]string{
		"no transaction object": "",
		"another hash":          "0x" + strings.Repeat("ab", 32),
	} {
		t.Run(name, func(t *testing.T) {
			w := newSweepRestartWorld(t)
			w.drive(&Checkpoint{SentSweeps: map[string]SentSweep{}})
			hash := w.mineFirstSent(t)
			w.l1.refuseMined = true
			w.l1.mu.Lock()
			w.l1.txByHash = map[string]string{hash: reported}
			w.l1.mu.Unlock()

			cp := &Checkpoint{SentSweeps: map[string]SentSweep{}}
			w.drive(cp)
			if got := cp.SentSweeps["0-7"]; len(got.TxHashes) != 0 || got.ConfirmTxId != "" {
				t.Fatalf("recorded %+v on a receipt the RPC cannot back, want nothing", got)
			}
			if n := confirmSweeps(t, w.l2); n != 0 {
				t.Fatalf("%d confirmSweep on an unbacked receipt, want 0", n)
			}
			// An honest answer: it is recorded and confirmed.
			w.l1.mu.Lock()
			w.l1.txByHash = nil
			w.l1.mu.Unlock()
			w.drive(cp)
			if got := cp.SentSweeps["0-7"]; len(got.TxHashes) != 1 || got.TxHashes[0] != hash || confirmSweeps(t, w.l2) != 1 {
				t.Fatalf("with an honest RPC: entry %+v, %d confirmSweeps; want it recorded and confirmed", got, confirmSweeps(t, w.l2))
			}
		})
	}
}

// The normal path is unchanged: a version that is not on chain is broadcast once
// and recorded; the chain check costs one receipt read for it, then nothing.
func TestSWEEPRESTART1_AnUnminedVersionIsStillBroadcastOnce(t *testing.T) {
	w := newSweepRestartWorld(t)
	cp := &Checkpoint{SentSweeps: map[string]SentSweep{}}
	w.drive(cp)
	w.drive(cp)
	got := cp.SentSweeps["0-7"]
	if w.sentCount() != 1 || len(got.TxHashes) != 1 || got.ConfirmTxId != "" {
		t.Fatalf("broadcasts %d, entry %+v; want one broadcast, recorded, not confirmed", w.sentCount(), got)
	}
	if n := confirmSweeps(t, w.l2); n != 0 {
		t.Fatalf("%d confirmSweep for an unmined sweep, want 0", n)
	}
}
