package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"testing"
	"time"

	"vsc-node/lib/dids"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
	gethrlp "github.com/ethereum/go-ethereum/rlp"
)

var wdTo = common.HexToAddress("0x00000000000000000000000000000000000000e2")

// queuedTx is a PendingSpend unsigned withdrawal (0x02 || rlp of the 9 fields).
func queuedTx(t *testing.T, feeCap int64) string {
	t.Helper()
	b, err := gethrlp.EncodeToBytes([]any{big.NewInt(wireChainID), uint64(0), big.NewInt(2e9), big.NewInt(feeCap), uint64(21000), wdTo, big.NewInt(1e18), []byte{}, types.AccessList{}})
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(append([]byte{0x02}, b...))
}

// tssSig is a DER signature over the queued tx's sighash, as getTssRequests returns.
func tssSig(t *testing.T, unsignedHex string) string {
	t.Helper()
	key, _ := ethCrypto.GenerateKey()
	sh, _ := computeSighash(unsignedHex)
	h, _ := hex.DecodeString(sh)
	sig, err := ethCrypto.Sign(h, key)
	if err != nil {
		t.Fatal(err)
	}
	enc := func(x []byte) []byte {
		x = new(big.Int).SetBytes(x).Bytes()
		if x[0]&0x80 != 0 {
			x = append([]byte{0}, x...)
		}
		return append([]byte{0x02, byte(len(x))}, x...)
	}
	body := append(enc(sig[:32]), enc(sig[32:64])...)
	return hex.EncodeToString(append([]byte{0x30, byte(len(body))}, body...))
}

func spendJSON(unsignedHex string) string {
	raw, _ := json.Marshal(contractPendingSpend{Nonce: 0, Amount: big.NewInt(1e18), From: "hive:alice", To: wdTo.Hex(), Asset: "eth",
		UnsignedTxHex: unsignedHex, BlockHeight: 100, VaultAtQueue: "0x00000000000000000000000000000000000000fa"})
	return string(raw)
}

func wdWorld(t *testing.T, unsignedHex string) (*fakeL1, *fakeL2, *ethRPC, *vscGraphQL, *Checkpoint, EVMBotConfig) {
	l1, l2 := newFakeL1(), newFakeL2()
	l2.set("vsc1bridge", "n", "0")
	l2.set("vsc1bridge", "np", "1")
	l2.set("vsc1bridge", "d-0", spendJSON(unsignedHex))
	sh, _ := computeSighash(unsignedHex)
	l2.sigs[sh] = tssSig(t, unsignedHex)
	cp := &Checkpoint{SentWithdrawals: map[string]SentTx{}, BlockRetries: map[uint64]int{}, PendingMaps: map[string]PendingMap{}}
	return l1, l2, l1.rpc(t), l2.gql(t), cp, EVMBotConfig{ContractID: "vsc1bridge", NetID: "vsc-devnet", RcLimit: 10000}
}

// B1: after an admin replaceWithdrawal re-signs nonce 0 at a higher fee, the
// bot broadcasts the replacement (it used to keep waiting on the old tx).
func TestB1_ReplacementIsBroadcast(t *testing.T) {
	oldTx, newTx := queuedTx(t, 40e9), queuedTx(t, 80e9)
	l1, _, rpc, gql, cp, cfg := wdWorld(t, newTx)
	cp.SentWithdrawals["0"] = SentTx{TxHash: "0xold", UnsignedTxHex: oldTx, Nonce: 0, SentAt: 1, LastBroadcastAt: time.Now().Unix()}
	handleWithdrawals(context.Background(), rpc, gql, cp, cfg, "vsc1bridge-primary")
	got := cp.SentWithdrawals["0"]
	if len(l1.sent) != 1 || got.UnsignedTxHex != newTx || got.TxHash == "0xold" {
		t.Fatalf("broadcasts %d, entry %+v: want the replacement broadcast and tracked", len(l1.sent), got)
	}
	if len(got.PrevTxHashes) != 1 || got.PrevTxHashes[0] != "0xold" || got.SentAt != 1 {
		t.Fatalf("entry %+v: want the old hash kept in PrevTxHashes and the original send time", got)
	}
}

// B8: a broadcast tx that never mines is re-posted every rebroadcastInterval
// (it used to be posted once); a mined one is not.
func TestB8_UnminedTxIsReposted(t *testing.T) {
	old := rebroadcastInterval
	rebroadcastInterval = 0
	t.Cleanup(func() { rebroadcastInterval = old })
	tx := queuedTx(t, 40e9)
	l1, _, rpc, gql, cp, cfg := wdWorld(t, tx)
	for i := 0; i < 3; i++ {
		handleWithdrawals(context.Background(), rpc, gql, cp, cfg, "vsc1bridge-primary")
	}
	if len(l1.sent) != 3 || l1.sent[1] != l1.sent[0] || l1.sent[2] != l1.sent[0] {
		t.Fatalf("broadcasts %d (%v), want the same signed tx 3 times", len(l1.sent), len(l1.sent) > 0)
	}

	// Mined: no more re-posts.
	b, _ := hex.DecodeString(l1.sent[0][2:])
	var mined types.Transaction
	if err := mined.UnmarshalBinary(b); err != nil {
		t.Fatal(err)
	}
	c := l1.block(200)
	rc := &types.Receipt{Type: types.DynamicFeeTxType, Status: 1, CumulativeGasUsed: 21000, TxHash: mined.Hash()}
	c.txs, c.receipts = append(c.txs, &mined), append(c.receipts, rc)
	handleWithdrawals(context.Background(), rpc, gql, cp, cfg, "vsc1bridge-primary")
	if len(l1.sent) != 3 {
		t.Fatalf("a mined tx was re-posted (%d broadcasts)", len(l1.sent))
	}
}

// LOW 17: entries for nonces the contract already closed are dropped.
func TestLOW17_ClosedNoncesAreDropped(t *testing.T) {
	_, l2, rpc, gql, cp, cfg := wdWorld(t, queuedTx(t, 40e9))
	l2.set("vsc1bridge", "n", "3")
	l2.set("vsc1bridge", "np", "3")
	cp.SentWithdrawals["1"] = SentTx{Nonce: 1, TxHash: "0x1"}
	cp.SentWithdrawals["2"] = SentTx{Nonce: 2, TxHash: "0x2"}
	handleWithdrawals(context.Background(), rpc, gql, cp, cfg, "vsc1bridge-primary")
	if len(cp.SentWithdrawals) != 0 {
		t.Fatalf("closed nonces kept: %+v", cp.SentWithdrawals)
	}
}

// confirmWorld: nonce 0's withdrawal mined in block 101, the verifier at 101.
func confirmWorld(t *testing.T) (*fakeL2, *ethRPC, *vscGraphQL, *Checkpoint, EVMBotConfig, *l2Submitter) {
	tx := queuedTx(t, 40e9)
	l1, l2, rpc, gql, cp, cfg := wdWorld(t, tx)
	l2.set("vsc1bridge", "zkverifier", "vsc1verifier")
	l2.set("vsc1verifier", "h", "101")
	vaultKey, _ := ethCrypto.GenerateKey()
	minedTx := l1.block(101).addTx(t, vaultKey, &types.DynamicFeeTx{Nonce: 0, GasTipCap: big.NewInt(2e9), GasFeeCap: big.NewInt(40e9), Gas: 21000, To: &wdTo, Value: big.NewInt(1e18)})
	cp.SentWithdrawals["0"] = SentTx{TxHash: minedTx.Hash().Hex(), UnsignedTxHex: tx, Nonce: 0, SentAt: 1, LastBroadcastAt: time.Now().Unix()}
	botKey, _ := ethCrypto.GenerateKey()
	sub := &l2Submitter{ethKey: botKey, did: dids.NewEthDID(ethCrypto.PubkeyToAddress(botKey.PublicKey).Hex()), gql: gql, cfg: cfg}
	return l2, rpc, gql, cp, cfg, sub
}

// B4 / B16: a submitted confirmSpend keeps the withdrawal until its result is
// CONFIRMED (it used to be dropped as soon as the pool took the call), and a
// FAILED one is re-sent only after a backoff (it used to be re-sent every
// couple of ticks, burning RC).
func TestB16_ConfirmSpendIsSettledByItsResultAndBacksOffOnFailure(t *testing.T) {
	l2, rpc, gql, cp, cfg, sub := confirmWorld(t)
	status := "UNCONFIRMED"
	l2.status = func(string) string { return status }
	tick := func() { handleConfirmations(context.Background(), rpc, gql, sub, cp, cfg) }

	tick()
	if len(l2.submitted) != 1 || cp.SentWithdrawals["0"].ConfirmTxId == "" {
		t.Fatalf("after the first tick: %d submissions, entry %+v; want one confirmSpend tracked", len(l2.submitted), cp.SentWithdrawals["0"])
	}
	tick() // still pending: no re-send
	if len(l2.submitted) != 1 {
		t.Fatalf("a pending confirmSpend was re-sent (%d submissions)", len(l2.submitted))
	}
	status = "FAILED"
	tick() // failure recorded, backoff starts
	tick() // inside the backoff: no re-send
	e := cp.SentWithdrawals["0"]
	if len(l2.submitted) != 1 || e.ConfirmFailures != 1 || e.ConfirmTxId != "" || e.NextConfirmAt <= time.Now().Unix() {
		t.Fatalf("after a FAILED confirmSpend: %d submissions, entry %+v; want one failure and a future retry", len(l2.submitted), e)
	}
	e.NextConfirmAt = time.Now().Add(-time.Second).Unix()
	cp.SentWithdrawals["0"] = e
	status = "UNCONFIRMED"
	tick() // backoff over: re-sent once
	if len(l2.submitted) != 2 {
		t.Fatalf("after the backoff: %d submissions, want 2", len(l2.submitted))
	}
	status = "CONFIRMED"
	tick()
	if _, ok := cp.SentWithdrawals["0"]; ok {
		t.Fatal("a CONFIRMED confirmSpend left the withdrawal tracked")
	}
}

func TestConfirmBackoffDoublesToAnHour(t *testing.T) {
	for n, want := range map[int]time.Duration{1: 5 * time.Minute, 2: 10 * time.Minute, 4: 40 * time.Minute, 5: time.Hour, 20: time.Hour} {
		if got := confirmBackoff(n); got != want {
			t.Errorf("confirmBackoff(%d) = %s, want %s", n, got, want)
		}
	}
}

// B1: when the replaced tx is the one that mines, the bot confirms it.
func TestB1_TheReplacedTxMiningStillSettles(t *testing.T) {
	l2, rpc, gql, cp, cfg, sub := confirmWorld(t)
	e := cp.SentWithdrawals["0"]
	minedHash := e.TxHash
	e.TxHash, e.PrevTxHashes = "0x00000000000000000000000000000000000000000000000000000000000000aa", []string{minedHash}
	cp.SentWithdrawals["0"] = e
	l2.status = func(string) string { return "UNCONFIRMED" }
	handleConfirmations(context.Background(), rpc, gql, sub, cp, cfg)
	if got := cp.SentWithdrawals["0"]; len(l2.submitted) != 1 || got.TxHash != minedHash || got.ConfirmTxId == "" {
		t.Fatalf("%d submissions, entry %+v; want confirmSpend for the mined (replaced) tx", len(l2.submitted), got)
	}
}

// B-R5: a checkpoint v1 entry (no UnsignedTxHex) adopted whatever tx the
// contract had queued, so a withdrawal an admin replaced before the upgrade
// was taken as already broadcast: the bot re-posted the old tx and never the
// replacement. The entry adopts the queued tx only if its signed tx is that tx.
func TestBR5_AV1EntryAdoptsOnlyTheTxItSigned(t *testing.T) {
	oldTx, newTx := queuedTx(t, 40e9), queuedTx(t, 80e9)
	signed := func(unsigned string) string {
		h, err := attachSignatureToTx(unsigned, 1, make([]byte, 32), append(make([]byte, 31), 7))
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	if !signedTxSigns(signed(oldTx), oldTx) || signedTxSigns(signed(oldTx), newTx) || signedTxSigns(oldTx, oldTx) {
		t.Fatal("signedTxSigns: want true only for a tx and its own signed form")
	}

	// Replaced before the upgrade: the replacement is broadcast.
	l1, _, rpc, gql, cp, cfg := wdWorld(t, newTx)
	cp.SentWithdrawals["0"] = SentTx{TxHash: "0xold", SignedTxHex: signed(oldTx), Nonce: 0, SentAt: 1, LastBroadcastAt: time.Now().Unix()}
	handleWithdrawals(context.Background(), rpc, gql, cp, cfg, "vsc1bridge-primary")
	got := cp.SentWithdrawals["0"]
	if len(l1.sent) != 1 || got.UnsignedTxHex != newTx || len(got.PrevTxHashes) != 1 || got.PrevTxHashes[0] != "0xold" {
		t.Fatalf("replaced v1 entry: %d broadcasts, entry %+v; want the replacement broadcast and the old hash kept", len(l1.sent), got)
	}

	// Not replaced: the entry keeps waiting on its own tx; nothing new is signed.
	l1, _, rpc, gql, cp, cfg = wdWorld(t, oldTx)
	cp.SentWithdrawals["0"] = SentTx{TxHash: "0xold", SignedTxHex: signed(oldTx), Nonce: 0, SentAt: 1, LastBroadcastAt: time.Now().Unix()}
	handleWithdrawals(context.Background(), rpc, gql, cp, cfg, "vsc1bridge-primary")
	got = cp.SentWithdrawals["0"]
	if len(l1.sent) != 0 || got.TxHash != "0xold" || len(got.PrevTxHashes) != 0 {
		t.Fatalf("unreplaced v1 entry: %d broadcasts, entry %+v; want it left waiting on its tx", len(l1.sent), got)
	}
}
