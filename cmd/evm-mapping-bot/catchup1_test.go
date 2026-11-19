package main

import (
	"context"
	"io"
	"math"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// EVM-CATCHUP-1 (testnet 11-18): an RPC rate limit that outlasted logsAtRetry's
// pauses failed a block of the scan window, and the scan stopped there until
// the next ~60 s tick, dropping the blocks after it. After a 3 h outage the
// bot stayed ~1,000 blocks behind for hours. Like the BTC bot (it retries the
// block it failed on after a short sleep and goes on), the failed block is now
// scanned again inside the tick, and the blocks after it are kept.
func TestEVMCATCHUP1_AThrottledBlockIsScannedAgainInTheSameTick(t *testing.T) {
	for _, tc := range []struct {
		name   string
		pauses []time.Duration
		want   uint64
	}{
		{"block retried in the tick", []time.Duration{2 * time.Second, 5 * time.Second}, 104},
		{"no retry (the old scan) holds at the throttled block", nil, 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := scanBlockRetryPauses
			scanBlockRetryPauses = tc.pauses
			t.Cleanup(func() { scanBlockRetryPauses = old })

			l1 := newFakeL1()
			l2, gql, rpc, sub, cfg, vault := bridgeWorld(t, l1, 104)
			// A token makes every block's scan read eth_getLogs.
			cfg.Tokens = map[string]string{"0x00000000000000000000000000000000000000c0": "usdc"}
			for n := uint64(101); n <= 104; n++ {
				l1.block(n)
			}
			ethDeposit(t, l1.block(103), vault, big.NewInt(1e18), nil)
			// Every eth_getLogs is refused for 2.5 s: longer than logsAtRetry's
			// 1.75 s, so the parallel scan fails all four blocks.
			l1.mu.Lock()
			l1.logs429Until = time.Now().Add(2500 * time.Millisecond)
			l1.mu.Unlock()

			cp := &Checkpoint{LastScannedBlock: 100, SentWithdrawals: map[string]SentTx{}, BlockRetries: map[uint64]int{}, PendingMaps: map[string]PendingMap{}}
			if err := runOnce(context.Background(), rpc, gql, sub, cp, cfg, "vsc1bridge-primary"); err != nil {
				t.Fatal(err)
			}
			if cp.LastScannedBlock != tc.want {
				t.Fatalf("checkpoint %d after one tick, want %d", cp.LastScannedBlock, tc.want)
			}
			found := len(l2.submitted) + len(cp.WaitingDeposits[103])
			if tc.want == 104 && found == 0 {
				t.Fatalf("block 103's deposit was scanned past without being parked or mapped (submitted %d, waiting %v)", len(l2.submitted), cp.WaitingDeposits)
			}
			if tc.want == 100 && found != 0 {
				t.Fatalf("with the scan held at 100, block 103's deposit was already handled (submitted %d)", len(l2.submitted))
			}
		})
	}
}

// A block that keeps failing past the retries still stops the scan (B-R2): a
// retry can only replace a failure with a success, never mark a block scanned.
func TestEVMCATCHUP1_ABlockThatKeepsFailingStillHoldsTheScan(t *testing.T) {
	old := scanBlockRetryPauses
	scanBlockRetryPauses = []time.Duration{10 * time.Millisecond, 10 * time.Millisecond}
	t.Cleanup(func() { scanBlockRetryPauses = old })

	l1 := newFakeL1()
	_, gql, rpc, sub, cfg, _ := bridgeWorld(t, l1, 102)
	cfg.Tokens = map[string]string{"0x00000000000000000000000000000000000000c0": "usdc"}
	l1.block(101)
	l1.block(102)
	l1.mu.Lock()
	l1.logs429Until = time.Now().Add(time.Hour)
	l1.mu.Unlock()

	cp := &Checkpoint{LastScannedBlock: 100, SentWithdrawals: map[string]SentTx{}, BlockRetries: map[uint64]int{}, PendingMaps: map[string]PendingMap{}}
	if err := runOnce(context.Background(), rpc, gql, sub, cp, cfg, "vsc1bridge-primary"); err != nil {
		t.Fatal(err)
	}
	if cp.LastScannedBlock != 100 {
		t.Fatalf("a block refused on every attempt was marked scanned: checkpoint %d, want 100", cp.LastScannedBlock)
	}
}

// v2 (testnet 11-19): the walk's retry had no time left once the parallel
// prefetch used the whole deposit phase under a rate limit, and every block
// fetched after the first failed one was thrown away and fetched again next
// tick. Blocks fetched once are now kept (cp.scanned) until the walk passes
// them, and only the rest are fetched.
func TestEVMCATCHUP1_FetchedBlocksAreKeptAcrossTicks(t *testing.T) {
	old := scanBlockRetryPauses
	scanBlockRetryPauses = []time.Duration{10 * time.Millisecond, 10 * time.Millisecond}
	t.Cleanup(func() { scanBlockRetryPauses = old })

	l1 := newFakeL1()
	_, gql, rpc, sub, cfg, _ := bridgeWorld(t, l1, 110)
	cfg.Tokens = map[string]string{"0x00000000000000000000000000000000000000c0": "usdc"}
	for n := uint64(101); n <= 110; n++ {
		l1.block(n)
	}
	l1.mu.Lock()
	l1.logs429Blocks = map[uint64]bool{101: true}
	l1.mu.Unlock()
	cp := &Checkpoint{LastScannedBlock: 100, SentWithdrawals: map[string]SentTx{}, BlockRetries: map[uint64]int{}, PendingMaps: map[string]PendingMap{}}
	if err := runOnce(context.Background(), rpc, gql, sub, cp, cfg, "vsc1bridge-primary"); err != nil {
		t.Fatal(err)
	}
	if cp.LastScannedBlock != 100 || len(cp.scanned) != 9 {
		t.Fatalf("tick 1 (block 101 refused): checkpoint %d, %d blocks kept; want 100 and 102-110 kept", cp.LastScannedBlock, len(cp.scanned))
	}

	l1.mu.Lock()
	l1.logs429Blocks, l1.fetched = nil, nil
	l1.mu.Unlock()
	if err := runOnce(context.Background(), rpc, gql, sub, cp, cfg, "vsc1bridge-primary"); err != nil {
		t.Fatal(err)
	}
	if cp.LastScannedBlock != 110 {
		t.Fatalf("tick 2: checkpoint %d, want 110", cp.LastScannedBlock)
	}
	l1.mu.Lock()
	defer l1.mu.Unlock()
	for n := uint64(102); n <= 110; n++ {
		if l1.fetched[n] != 0 {
			t.Errorf("block %d was fetched again on tick 2 (%d times), want it taken from tick 1", n, l1.fetched[n])
		}
	}
	if l1.fetched[101] == 0 {
		t.Errorf("block 101 (refused on tick 1) was not fetched on tick 2")
	}
	if len(cp.scanned) != 0 {
		t.Errorf("%d kept blocks left behind the checkpoint", len(cp.scanned))
	}
}

// A kept block was scanned for the vault set of its tick. When a vault
// generation is added, a deposit to it in a block fetched before must still be
// found: the kept blocks are dropped and fetched again for the new set.
func TestEVMCATCHUP1_ANewVaultScansKeptBlocksAgain(t *testing.T) {
	old := scanBlockRetryPauses
	scanBlockRetryPauses = []time.Duration{10 * time.Millisecond, 10 * time.Millisecond}
	t.Cleanup(func() { scanBlockRetryPauses = old })

	l1 := newFakeL1()
	_, gql, rpc, sub, cfg, vaultA := bridgeWorld(t, l1, 101) // header of 102 not stored: its deposit stays parked
	cfg.Tokens = map[string]string{"0x00000000000000000000000000000000000000c0": "usdc"}
	vaultB := common.HexToAddress("0x00000000000000000000000000000000000ab1e5")
	l1.block(101)
	ethDeposit(t, l1.block(102), vaultB, big.NewInt(1e18), nil)
	cfg.Vaults = []string{strings.ToLower(vaultA.Hex())}
	l1.mu.Lock()
	l1.logs429Blocks = map[uint64]bool{101: true}
	l1.mu.Unlock()
	cp := &Checkpoint{LastScannedBlock: 100, SentWithdrawals: map[string]SentTx{}, BlockRetries: map[uint64]int{}, PendingMaps: map[string]PendingMap{}}
	if err := runOnce(context.Background(), rpc, gql, sub, cp, cfg, "vsc1bridge-primary"); err != nil {
		t.Fatal(err)
	}
	if _, kept := cp.scanned[102]; !kept || cp.LastScannedBlock != 100 {
		t.Fatalf("tick 1: checkpoint %d, block 102 kept %v; want 100 and 102 kept (scanned for vault A only)", cp.LastScannedBlock, kept)
	}

	l1.mu.Lock()
	l1.logs429Blocks = nil
	l1.mu.Unlock()
	cfg.Vaults = []string{strings.ToLower(vaultA.Hex()), strings.ToLower(vaultB.Hex())}
	if err := runOnce(context.Background(), rpc, gql, sub, cp, cfg, "vsc1bridge-primary"); err != nil {
		t.Fatal(err)
	}
	if cp.LastScannedBlock != 102 || len(cp.WaitingDeposits[102]) != 1 {
		t.Fatalf("tick 2 with vault B added: checkpoint %d, deposits parked at 102: %v; want the deposit to vault B found", cp.LastScannedBlock, cp.WaitingDeposits[102])
	}
}

// rateLimitRPC is an L1 RPC behind a token-bucket rate limit: a call with no
// token left is answered HTTP 429, like a throttled provider.
type rateLimitRPC struct {
	mu             sync.Mutex
	rate, burst    float64
	tokens         float64
	last           time.Time
	calls, refused atomic.Int64
}

func (r *rateLimitRPC) RoundTrip(req *http.Request) (*http.Response, error) {
	r.calls.Add(1)
	r.mu.Lock()
	now := time.Now()
	if r.last.IsZero() {
		r.last, r.tokens = now, r.burst
	}
	r.tokens = math.Min(r.burst, r.tokens+now.Sub(r.last).Seconds()*r.rate)
	r.last = now
	ok := r.tokens >= 1
	if ok {
		r.tokens--
	}
	r.mu.Unlock()
	if !ok {
		r.refused.Add(1)
		return &http.Response{StatusCode: http.StatusTooManyRequests, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}, Request: req}, nil
	}
	return http.DefaultTransport.RoundTrip(req)
}

// The live symptom in small: a 120-block backlog behind an RPC that serves 6
// calls a second (most calls refused, as on testnet 11-18). In this harness
// the old scan (develop 30c660cd) moved 100 -> 110 in 8 ticks, about a block a
// tick, like the bot that stayed ~1,000 blocks behind; this scan moves ~6 a
// tick, about what 6 calls a second allow.
func TestEVMCATCHUP1_ABacklogClearsBehindARateLimit(t *testing.T) {
	oldB, oldP, oldR := depositPhaseBudget, scanBlockRetryPauses, scanWalkReserve
	depositPhaseBudget, scanBlockRetryPauses, scanWalkReserve = 3*time.Second, []time.Duration{200 * time.Millisecond, 500 * time.Millisecond}, time.Second
	t.Cleanup(func() { depositPhaseBudget, scanBlockRetryPauses, scanWalkReserve = oldB, oldP, oldR })

	l1 := newFakeL1()
	_, gql, rpc, sub, cfg, _ := bridgeWorld(t, l1, 220)
	cfg.Tokens = map[string]string{"0x00000000000000000000000000000000000000c0": "usdc"}
	for n := uint64(101); n <= 220; n++ {
		l1.block(n)
	}
	lim := &rateLimitRPC{rate: 6, burst: 6}
	rpc.client.Transport = lim
	cp := &Checkpoint{LastScannedBlock: 100, SentWithdrawals: map[string]SentTx{}, BlockRetries: map[uint64]int{}, PendingMaps: map[string]PendingMap{}}
	var progress []uint64
	for tick := 0; tick < 8 && cp.LastScannedBlock < 220; tick++ {
		_ = runOnce(context.Background(), rpc, gql, sub, cp, cfg, "vsc1bridge-primary") // a refused finalized read just ends the tick, as in the bot
		progress = append(progress, cp.LastScannedBlock)
	}
	t.Logf("checkpoint after each tick: %v (%d RPC calls, %d refused)", progress, lim.calls.Load(), lim.refused.Load())
	if cp.LastScannedBlock < 135 {
		t.Fatalf("8 ticks behind a 6 calls/s RPC moved the checkpoint only to %d (%v), want at least 135", cp.LastScannedBlock, progress)
	}
}
