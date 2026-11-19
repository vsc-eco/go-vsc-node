package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"vsc-node/lib/dids"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
	gethrlp "github.com/ethereum/go-ethereum/rlp"
)

// rp9Chain serves headers x..tip-1 over a fake L1 RPC and maps each header's
// RLP back to its number.
func rp9Chain(t *testing.T, x, tip uint64) (*ethRPC, map[string]uint64) {
	t.Helper()
	byNum := map[uint64]map[string]any{}
	numOf := map[string]uint64{}
	parent := common.Hash{}
	for n := x; n < tip; n++ {
		zero, root := uint64(0), common.HexToHash("0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421")
		h := &types.Header{ParentHash: parent, Number: new(big.Int).SetUint64(n), GasLimit: 30_000_000, Time: 1_700_000_000 + n,
			Difficulty: new(big.Int), BaseFee: big.NewInt(1e9), WithdrawalsHash: &root, BlobGasUsed: &zero, ExcessBlobGas: &zero, ParentBeaconRoot: &root,
			UncleHash: types.EmptyUncleHash, Root: root, TxHash: root, ReceiptHash: root}
		j, _ := json.Marshal(h)
		var m map[string]any
		_ = json.Unmarshal(j, &m)
		byNum[n] = m
		rlpb, _ := gethrlp.EncodeToBytes(h)
		numOf[hex.EncodeToString(rlpb)] = n
		parent = h.Hash()
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Params []json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var hexN string
		_ = json.Unmarshal(req.Params[0], &hexN)
		n, _ := strconv.ParseUint(strings.TrimPrefix(hexN, "0x"), 16, 64)
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": byNum[n]})
	}))
	t.Cleanup(srv.Close)
	return newEthRPC(srv.URL), numOf
}

// rp9Sent is every block number the submitHeaders calls carried, in call order.
func rp9Sent(t *testing.T, l2 *fakeL2, numOf map[string]uint64) [][]uint64 {
	t.Helper()
	var runs [][]uint64
	for _, c := range submittedCalls(t, l2) {
		var p struct {
			Headers []struct {
				RLP string `json:"rlp_hex"`
			} `json:"headers"`
		}
		if c.Action != "submitHeaders" || json.Unmarshal([]byte(c.Payload), &p) != nil {
			t.Fatalf("unexpected call %s %q", c.Action, c.Payload)
		}
		var run []uint64
		for _, h := range p.Headers {
			run = append(run, numOf[h.RLP])
		}
		runs = append(runs, run)
	}
	return runs
}

func rp9Submitter(t *testing.T, l2 *fakeL2) *l2Submitter {
	bk, _ := ethCrypto.GenerateKey()
	cfg := EVMBotConfig{ContractID: "vsc1bridge", NetID: "vsc-devnet", RcLimit: 10000}
	return &l2Submitter{ethKey: bk, did: dids.NewEthDID(ethCrypto.PubkeyToAddress(bk.PublicKey).Hex()), gql: l2.gql(t), cfg: cfg}
}

// RP9-LINKDOWN-RESTART (testnet 10-31): a partial link-down left blocks
// 510..519 stored under the proven tip 520. The next one links only 500..509,
// down from 510, instead of re-sending (and re-paying) 510..519.
func TestRP9_LinkDownResumesBelowTheStoredRun(t *testing.T) {
	const x, tip = uint64(500), uint64(520)
	rpc, numOf := rp9Chain(t, x, tip)
	l2 := newFakeL2()
	for n := uint64(510); n < tip; n++ {
		l2.set("vsc1verifier", "b-"+strconv.FormatUint(n, 10), strings.Repeat("\x00", 137))
	}
	linkDown(context.Background(), rpc, rp9Submitter(t, l2), "vsc1verifier", x, tip)
	runs := rp9Sent(t, l2, numOf)
	seen := map[uint64]bool{}
	next := uint64(509) // newest first: each run ends right under the one above it
	for _, run := range runs {
		if top := run[len(run)-1]; top != next {
			t.Fatalf("a run ends at %d, want %d (runs %v)", top, next, runs)
		}
		for _, n := range run {
			if n >= 510 {
				t.Fatalf("re-sent stored block %d (runs %v)", n, runs)
			}
			seen[n] = true
		}
		next = run[0] - 1
	}
	if len(seen) != 10 {
		t.Fatalf("sent %d blocks, want 500..509 (runs %v)", len(seen), runs)
	}
}

// With RC for one call (45k available, 30k a call) one run goes out, the newest
// (514..519), and the rest waits for the next tick instead of queueing calls the
// node cannot include (they held every later bot nonce on testnet).
func TestRP9_LinkDownSendsOnlyWhatRCCovers(t *testing.T) {
	const x, tip = uint64(500), uint64(520)
	rpc, numOf := rp9Chain(t, x, tip)
	l2 := newFakeL2()
	sub := rp9Submitter(t, l2)
	l2.rc = map[string]int64{sub.did.String(): 45_000}
	linkDown(context.Background(), rpc, sub, "vsc1verifier", x, tip)
	runs := rp9Sent(t, l2, numOf)
	if len(runs) != 1 || runs[0][len(runs[0])-1] != 519 {
		t.Fatalf("runs %v, want one run ending at 519", runs)
	}
}

// Nothing stored and RC enough for every run: unchanged, all of x..tip-1.
func TestRP9_LinkDownWholeGapWhenRCSuffices(t *testing.T) {
	const x, tip = uint64(500), uint64(520)
	rpc, numOf := rp9Chain(t, x, tip)
	l2 := newFakeL2()
	sub := rp9Submitter(t, l2)
	l2.rc = map[string]int64{sub.did.String(): 1_000_000}
	linkDown(context.Background(), rpc, sub, "vsc1verifier", x, tip)
	n := 0
	for _, run := range rp9Sent(t, l2, numOf) {
		n += len(run)
	}
	if n != int(tip-x) {
		t.Fatalf("sent %d headers, want %d", n, tip-x)
	}
}
