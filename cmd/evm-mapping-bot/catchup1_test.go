package main

import (
	"context"
	"math/big"
	"testing"
	"time"
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
