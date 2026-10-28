package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"vsc-node/lib/dids"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
	gethrlp "github.com/ethereum/go-ethereum/rlp"
)

// slowRPC delays every L1 RPC call and counts the calls and the most in flight
// at once.
type slowRPC struct {
	delay            time.Duration
	calls, now, peak atomic.Int64
}

func (s *slowRPC) RoundTrip(r *http.Request) (*http.Response, error) {
	s.calls.Add(1)
	n := s.now.Add(1)
	defer s.now.Add(-1)
	for p := s.peak.Load(); n > p && !s.peak.CompareAndSwap(p, n); p = s.peak.Load() {
	}
	select {
	case <-time.After(s.delay):
	case <-r.Context().Done():
		return nil, r.Context().Err()
	}
	return http.DefaultTransport.RoundTrip(r)
}

// tickWorld: a bridge whose withdrawal at nonce 0 mined in block 101 and is
// ready to confirm, the verifier at tip, and the bot's L1 RPC slowed by slow.
func tickWorld(t *testing.T, tip uint64, slow *slowRPC) (*fakeL1, *fakeL2, *ethRPC, *vscGraphQL, *Checkpoint, EVMBotConfig, *l2Submitter, common.Address) {
	t.Helper()
	tx := queuedTx(t, 40e9)
	l1, l2, rpc, gql, cp, cfg := wdWorld(t, tx)
	l2.status = func(string) string { return "UNCONFIRMED" }
	l2.set("vsc1bridge", "zkverifier", "vsc1verifier")
	l2.set("vsc1verifier", "h", big.NewInt(int64(tip)).String())
	mk, _ := ethCrypto.GenerateKey()
	mined := l1.block(101).addTx(t, mk, &types.DynamicFeeTx{Nonce: 0, GasTipCap: big.NewInt(2e9), GasFeeCap: big.NewInt(40e9), Gas: 21000, To: &wdTo, Value: big.NewInt(1e18)})
	cp.SentWithdrawals["0"] = SentTx{TxHash: mined.Hash().Hex(), UnsignedTxHex: tx, Nonce: 0, SentAt: 1, LastBroadcastAt: time.Now().Unix()}
	vk, _ := ethCrypto.GenerateKey()
	vault := ethCrypto.PubkeyToAddress(vk.PublicKey)
	cfg.VaultAddress, cfg.Tokens = strings.ToLower(vault.Hex()), map[string]string{}
	bk, _ := ethCrypto.GenerateKey()
	sub := &l2Submitter{ethKey: bk, did: dids.NewEthDID(ethCrypto.PubkeyToAddress(bk.PublicKey).Hex()), gql: gql, cfg: cfg}
	cp.LastScannedBlock = 101
	rpc.client.Transport = slow
	return l1, l2, rpc, gql, cp, cfg, sub, vault
}

// busyBlock fills block n with txs transfers, those at depAt to the vault
// (ETH deposits), and returns the deposits' hashes.
func busyBlock(t *testing.T, c *fakeChain, txs int, vault common.Address, depAt ...int) []string {
	t.Helper()
	k, _ := ethCrypto.GenerateKey()
	other := ethCrypto.PubkeyToAddress(k.PublicKey)
	dep := map[int]bool{}
	for _, i := range depAt {
		dep[i] = true
	}
	var hashes []string
	for i := 0; i < txs; i++ {
		to := other
		if dep[i] {
			to = vault
		}
		tx := c.addTx(t, k, &types.DynamicFeeTx{Nonce: uint64(i), GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(30e9), Gas: 21000, To: &to, Value: big.NewInt(1e15)})
		if dep[i] {
			hashes = append(hashes, tx.Hash().Hex())
		}
	}
	return hashes
}

func withDepositBudget(t *testing.T, d time.Duration) {
	old := depositPhaseBudget
	depositPhaseBudget = d
	t.Cleanup(func() { depositPhaseBudget = old })
}

func mappedDeposits(cp *Checkpoint, hashes []string) int {
	got := map[string]bool{}
	for _, pm := range cp.PendingMaps {
		got[pm.DepositTx] = true
	}
	n := 0
	for _, h := range hashes {
		if got[h] {
			n++
		}
	}
	return n
}

// TICKSTALL-1: each deposit's proof was built with 2N+1 serial RPC calls of
// its own, past the tick's deadline. Five ETH deposits in a 300-tx block never
// finished (the fifth was never mapped, the first four re-sent every tick) and
// the overrun starved confirmSpend. Scaled 10x down: 2 ms a call, a 6 s tick
// with a 3 s deposit budget (production: 60 s and 30 s; 20 ms is a fast RPC).
// The block's material is now fetched once, in parallel: the block finishes in
// one tick and the withdrawal is confirmed in the same tick.
func TestTICKSTALL1_BusyBlockFinishesInOneTickAndConfirmSpendRuns(t *testing.T) {
	withDepositBudget(t, 3*time.Second)
	slow := &slowRPC{delay: 2 * time.Millisecond}
	l1, _, rpc, gql, cp, cfg, sub, vault := tickWorld(t, 102, slow)
	deps := busyBlock(t, l1.block(102), 300, vault, 50, 100, 150, 200, 250)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	if err := runOnce(ctx, rpc, gql, sub, cp, cfg, "vsc1bridge-primary"); err != nil {
		t.Fatal(err)
	}
	if n := mappedDeposits(cp, deps); n != 5 || cp.LastScannedBlock != 102 {
		t.Fatalf("after one tick: %d/5 deposits mapped, last scanned %d; want 5 and 102", n, cp.LastScannedBlock)
	}
	if cp.SentWithdrawals["0"].ConfirmTxId == "" {
		t.Fatal("the ready withdrawal's confirmSpend was not submitted in the tick")
	}
	// One fetch of the block's 300 raw txs and 300 receipts serves all five.
	if c := slow.calls.Load(); c > 2*601 {
		t.Fatalf("%d L1 RPC calls for 5 deposits in a 300-tx block; one shared fetch is ~610", c)
	}
	if p := slow.peak.Load(); p < 2 {
		t.Fatalf("at most %d L1 call in flight: the block's fetch is serial", p)
	}
	if len(cp.BlockMapped) != 0 || len(cp.BlockRetries) != 0 {
		t.Fatalf("finished block left progress %v / retries %v", cp.BlockMapped, cp.BlockRetries)
	}
}

// TICKSTALL-1: a block retried after some of its maps were submitted sends
// only the others, then forgets its progress once finished.
func TestTICKSTALL1_RetriedBlockSkipsSubmittedDeposits(t *testing.T) {
	slow := &slowRPC{}
	l1, l2, rpc, gql, cp, cfg, sub, vault := tickWorld(t, 102, slow)
	deps := busyBlock(t, l1.block(102), 20, vault, 3, 9, 15)
	cp.BlockMapped = map[uint64][]string{102: {depositKey(detectedDeposit{TxHash: deps[0], LogIndex: -1})}}
	before := len(l2.submitted)

	if err := runOnce(context.Background(), rpc, gql, sub, cp, cfg, "vsc1bridge-primary"); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, pm := range cp.PendingMaps {
		got[pm.DepositTx] = true
	}
	// The tick also submits the confirmSpend: maps = submissions - 1.
	if maps := len(l2.submitted) - before - 1; maps != 2 || got[deps[0]] || !got[deps[1]] || !got[deps[2]] {
		t.Fatalf("%d maps submitted, pending %v; want the two not yet submitted (%s, %s)", maps, got, deps[1], deps[2])
	}
	if cp.LastScannedBlock != 102 || cp.BlockMapped != nil && len(cp.BlockMapped[102]) != 0 {
		t.Fatalf("last scanned %d, progress %v; want the block finished and its progress dropped", cp.LastScannedBlock, cp.BlockMapped)
	}
}

// TICKSTALL-1: deposits get at most depositPhaseBudget of the tick. Twenty
// parked deposit blocks (100 txs each) take longer than the tick to map; the ready
// confirmSpend still goes out in the first tick, the blocks continue over the
// next ticks, and a block given no time does not count as failing.
func TestTICKSTALL1_DepositBudgetLeavesTimeForConfirmations(t *testing.T) {
	withDepositBudget(t, 600*time.Millisecond)
	slow := &slowRPC{delay: 2 * time.Millisecond}
	l1, _, rpc, gql, cp, cfg, sub, vault := tickWorld(t, 121, slow)
	cp.WaitingDeposits = map[uint64][]detectedDeposit{}
	var all []string
	for h := uint64(102); h <= 121; h++ {
		all = append(all, busyBlock(t, l1.block(h), 100, vault, 7)...)
		res, err := scanBlock(rpc, h, cfg.VaultAddress, cfg.Tokens)
		if err != nil || len(res.Deposits) != 1 {
			t.Fatalf("scan block %d: %v %+v", h, err, res)
		}
		cp.WaitingDeposits[h] = res.Deposits
	}
	cp.LastScannedBlock = 121

	for tick := 1; tick <= 40 && mappedDeposits(cp, all) < len(all); tick++ {
		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		t0 := time.Now()
		if err := runOnce(ctx, rpc, gql, sub, cp, cfg, "vsc1bridge-primary"); err != nil {
			t.Fatalf("tick %d: %v", tick, err)
		}
		cancel()
		t.Logf("tick %d: %s, %d/%d mapped", tick, time.Since(t0).Round(10*time.Millisecond), mappedDeposits(cp, all), len(all))
		if tick == 1 {
			if cp.SentWithdrawals["0"].ConfirmTxId == "" {
				t.Fatal("tick 1: the deposit phase starved the ready confirmSpend")
			}
			if n := mappedDeposits(cp, all); n == 0 || n == len(all) {
				t.Fatalf("tick 1 mapped %d/%d deposits; the case needs a deposit phase longer than its budget", n, len(all))
			}
		}
		for h, r := range cp.BlockRetries {
			t.Fatalf("tick %d: block %d counted %d failed retries; it was only out of time", tick, h, r)
		}
	}
	if n := mappedDeposits(cp, all); n != len(all) || len(cp.WaitingDeposits) != 0 {
		t.Fatalf("%d/%d deposits mapped, %d blocks still waiting", n, len(all), len(cp.WaitingDeposits))
	}
}

func TestTICKSTALL1_ForEachParallel(t *testing.T) {
	var seen [100]atomic.Bool
	if err := forEachParallel(context.Background(), 100, 8, func(i int) error { seen[i].Store(true); return nil }); err != nil {
		t.Fatal(err)
	}
	for i := range seen {
		if !seen[i].Load() {
			t.Fatalf("index %d never ran", i)
		}
	}
	boom := errors.New("boom")
	if err := forEachParallel(context.Background(), 100, 8, func(i int) error {
		if i == 40 {
			return boom
		}
		return nil
	}); !errors.Is(err, boom) {
		t.Fatalf("err %v, want the failing index's", err)
	}
	if err := forEachParallel(context.Background(), 10, 3, func(i int) error {
		if i == 5 {
			var m map[string]int
			m["x"] = 1 // a panic on hostile data
		}
		return nil
	}); err == nil || !strings.Contains(err.Error(), "panic at index 5") {
		t.Fatalf("a panicking index: err %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var ran atomic.Int64
	err := forEachParallel(ctx, 1000, 4, func(i int) error {
		if ran.Add(1) == 10 {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) || ran.Load() > 20 {
		t.Fatalf("after cancel: err %v, %d ran", err, ran.Load())
	}
}

// A call still waiting when its context ends is abandoned at once, and the
// next endpoint is not tried for it.
func TestTICKSTALL1_RPCCallHonoursItsContext(t *testing.T) {
	var hits atomic.Int64
	slowSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case <-time.After(1500 * time.Millisecond):
		case <-r.Context().Done():
		}
	}))
	defer slowSrv.Close()
	rpc := newEthRPC(slowSrv.URL + "," + slowSrv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	t0 := time.Now()
	_, err := rpc.callCtx(ctx, "eth_blockNumber", "")
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(t0) > time.Second || hits.Load() != 1 {
		t.Fatalf("err %v after %s, %d endpoint hits; want the deadline at once and one hit", err, time.Since(t0), hits.Load())
	}
}

// Checkpoint v3 (BlockMapped) loads v2 files and keeps its progress across a
// restart.
func TestTICKSTALL1_CheckpointKeepsBlockProgress(t *testing.T) {
	p := t.TempDir() + "/cp.json"
	if err := writeFileForTest(p, `{"last_scanned_block":9,"sent_withdrawals":{},"version":2}`); err != nil {
		t.Fatal(err)
	}
	if cp := loadCheckpoint(p); cp.LastScannedBlock != 9 || cp.Version != checkpointVersion {
		t.Fatalf("v2 file loaded as %+v", cp)
	}
	cp := loadCheckpoint(p)
	markMapped(cp, 10, detectedDeposit{TxHash: "0xAB", LogIndex: -1})
	cp.save(p)
	if got := loadCheckpoint(p).BlockMapped[10]; len(got) != 1 || got[0] != "0xab#-1" {
		t.Fatalf("progress after reload %v", got)
	}
}

// The other side of the budget: a block whose own proof fetch does not fit in
// the deposit phase still counts its failed retries, so the stuck-block alarm
// (blockRetryAlertThreshold) fires instead of a silent stall.
func TestTICKSTALL1_BlockThatCannotFitIsCounted(t *testing.T) {
	withDepositBudget(t, 50*time.Millisecond)
	slow := &slowRPC{delay: 5 * time.Millisecond}
	l1, _, rpc, gql, cp, cfg, sub, vault := tickWorld(t, 102, slow)
	busyBlock(t, l1.block(102), 300, vault, 10)
	for tick := 1; tick <= 3; tick++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if err := runOnce(ctx, rpc, gql, sub, cp, cfg, "vsc1bridge-primary"); err != nil {
			t.Fatal(err)
		}
		cancel()
	}
	if cp.BlockRetries[102] != 3 || len(cp.WaitingDeposits[102]) != 1 {
		t.Fatalf("retries %v, waiting %v; want block 102 parked and counted on each of 3 ticks", cp.BlockRetries, cp.WaitingDeposits)
	}
	if cp.SentWithdrawals["0"].ConfirmTxId == "" {
		t.Fatal("the stuck block starved the ready confirmSpend")
	}
}

// methodCount counts L1 RPC requests whose body contains want.
type methodCount struct {
	want string
	n    atomic.Int64
}

func (m *methodCount) RoundTrip(r *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(r.Body)
	if strings.Contains(string(body), m.want) {
		m.n.Add(1)
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return http.DefaultTransport.RoundTrip(r)
}

// refuseSubmits makes every L2 transaction submission fail.
type refuseSubmits struct{}

func (refuseSubmits) RoundTrip(r *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(r.Body)
	if strings.Contains(string(body), "submitTransactionV1") {
		return nil, errors.New("submission refused")
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return http.DefaultTransport.RoundTrip(r)
}

// TICKSTALL-1: a deposit block is scanned once. One whose maps cannot go out
// (here: every L2 submission fails) stays parked and is not scanned again, so
// a block packed with deposits does not spend every tick re-reading them.
func TestTICKSTALL1_DepositBlockIsScannedOnce(t *testing.T) {
	withDepositBudget(t, 300*time.Millisecond) // cuts each failed submission's retry backoff
	l1, _, rpc, gql, cp, cfg, sub, vault := tickWorld(t, 102, &slowRPC{})
	busyBlock(t, l1.block(102), 30, vault, 4, 8, 12)
	mc := &methodCount{want: `"eth_getBlockByNumber","params":["0x66", true]`}
	rpc.client.Transport = mc
	gql.client.Transport = refuseSubmits{}
	delete(cp.SentWithdrawals, "0") // only the deposits submit here
	for tick := 1; tick <= 3; tick++ {
		_ = runOnce(context.Background(), rpc, gql, sub, cp, cfg, "vsc1bridge-primary")
	}
	if n := mc.n.Load(); n != 1 || len(cp.WaitingDeposits[102]) != 3 || cp.BlockRetries[102] != 3 {
		t.Fatalf("block 102 scanned %d times, waiting %v, retries %v; want one scan, its 3 deposits parked, 3 failed ticks", n, cp.WaitingDeposits, cp.BlockRetries)
	}
}

// TICKSTALL-1 (sibling): the EIP-2935 prefetch runs at every new finalized
// block and fetched one header per needed block serially, so a backlog after
// an operator outage could outlast every tick. It is parallel and bounded by
// ctx; headers not fetched in time are left for a later root.
func TestTICKSTALL1_HistoryPrefetchIsParallelAndBounded(t *testing.T) {
	raw, err := os.ReadFile("testdata/headers.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx map[string]struct {
		Block map[string]any `json:"block"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	block := fx["sepolia"].Block
	const needs = 40
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var result any = block // every header request: one real Sepolia header
		if req.Method == "eth_getProof" {
			sp := make([]map[string]any, needs)
			for i := range sp {
				sp[i] = map[string]any{"proof": []string{"0xc0"}}
			}
			result = map[string]any{"accountProof": []string{"0xc0"}, "storageProof": sp}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
	}))
	defer srv.Close()
	const f = uint64(10_000)
	var open []uint64
	for i := uint64(0); i < needs; i++ {
		open = append(open, f-100+i)
	}

	slow := &slowRPC{delay: 10 * time.Millisecond}
	rpc := newEthRPC(srv.URL)
	rpc.client.Transport = slow
	e := prefetchHistoryProofs(context.Background(), rpc, f, open)
	if e == nil || e.Err != "" || len(e.Headers) != needs || slow.peak.Load() < 2 {
		t.Fatalf("prefetch: %+v headers %d, peak in flight %d; want all %d, fetched in parallel", e, len(e.Headers), slow.peak.Load(), needs)
	}

	// Header calls that hang: the prefetch returns at its deadline, abandoning
	// the calls in flight, with the headers left for a later root.
	rpc.client.Transport = &slowMethod{method: "eth_getBlockByNumber", delay: 3 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	t0 := time.Now()
	e = prefetchHistoryProofs(ctx, rpc, f, open)
	if el := time.Since(t0); el > time.Second || e == nil || len(e.Headers) == needs {
		t.Fatalf("prefetch past its deadline: %s, %d headers; want it cut short with some left for a later root", el, len(e.Headers))
	}
}

// slowMethod delays only the L1 RPC requests for method.
type slowMethod struct {
	method string
	delay  time.Duration
}

func (m *slowMethod) RoundTrip(r *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	if strings.Contains(string(body), `"`+m.method+`"`) {
		select {
		case <-time.After(m.delay):
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	}
	return http.DefaultTransport.RoundTrip(r)
}

// TICKSTALL-1 (sibling): linkDown fetches the run's headers in parallel and
// still sends every block x..tip-1 exactly once, newest run first, each run
// contiguous (each links to the parent hash the run above it stored).
func TestTICKSTALL1_LinkDownSendsEveryHeaderNewestFirst(t *testing.T) {
	const x, tip = uint64(500), uint64(520)
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
	defer srv.Close()
	slow := &slowRPC{delay: 5 * time.Millisecond}
	rpc := newEthRPC(srv.URL)
	rpc.client.Transport = slow
	l2 := newFakeL2()
	bk, _ := ethCrypto.GenerateKey()
	cfg := EVMBotConfig{ContractID: "vsc1bridge", NetID: "vsc-devnet", RcLimit: 10000}
	sub := &l2Submitter{ethKey: bk, did: dids.NewEthDID(ethCrypto.PubkeyToAddress(bk.PublicKey).Hex()), gql: l2.gql(t), cfg: cfg}

	linkDown(context.Background(), rpc, sub, "vsc1verifier", x, tip)

	calls := submittedCalls(t, l2)
	seen := map[uint64]int{}
	prevLow := tip
	for _, c := range calls {
		var p struct {
			Headers []struct {
				RLP string `json:"rlp_hex"`
			} `json:"headers"`
		}
		if c.Action != "submitHeaders" || json.Unmarshal([]byte(c.Payload), &p) != nil || len(p.Headers) == 0 {
			t.Fatalf("unexpected call %s %q", c.Action, c.Payload)
		}
		lo := numOf[p.Headers[0].RLP]
		for i, h := range p.Headers {
			n, ok := numOf[h.RLP]
			if !ok || n != lo+uint64(i) {
				t.Fatalf("run starting at %d: header %d is block %d (known %v); want a contiguous run", lo, i, n, ok)
			}
			seen[n]++
		}
		if top := lo + uint64(len(p.Headers)) - 1; top != prevLow-1 {
			t.Fatalf("run %d..%d is not the one just below the previous run (from %d)", lo, top, prevLow)
		}
		prevLow = lo
	}
	if prevLow != x || len(seen) != int(tip-x) {
		t.Fatalf("sent %d distinct blocks down to %d; want every block %d..%d", len(seen), prevLow, x, tip-1)
	}
	for n, k := range seen {
		if k != 1 {
			t.Fatalf("block %d sent %d times", n, k)
		}
	}
	if slow.peak.Load() < 2 {
		t.Fatalf("headers fetched one at a time (peak %d in flight)", slow.peak.Load())
	}
}
