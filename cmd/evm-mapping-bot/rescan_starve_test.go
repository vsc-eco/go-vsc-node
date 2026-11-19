package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// RESCAN-STARVE-1 (devnet BB2, 2026-11-13): the bot restarted with an empty
// checkpoint and re-scanned from its start block through slow, faulting RPCs.
// Scanning ran first in the deposit phase and used all of it every tick, so
// mapWaitingDeposits only ever got an expired context: a deposit already parked
// with its header stored was not mapped for the hour the re-scan took, and
// retireVault waited on it the whole time (I-26). Here: a deposit is parked at
// block 102, then the chain moves 5,000 blocks ahead and the deposit's header is
// stored; the next tick has a long scan backlog and a slow RPC.
func TestRESCANSTARVE1_ParkedDepositMapsWhileTheScanIsFarBehind(t *testing.T) {
	withDepositBudget(t, 300*time.Millisecond)
	fast := &slowRPC{delay: time.Millisecond}
	l1, l2, rpc, gql, cp, cfg, sub, vault := tickWorld(t, 101, fast)
	deps := busyBlock(t, l1.block(102), 3, vault, 1)

	// Tick 1: the deposit is found and parked (its header is not stored yet).
	ctx1, cancel1 := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel1()
	if err := runOnce(ctx1, rpc, gql, sub, cp, cfg, "vsc1bridge-primary"); err != nil {
		t.Fatal(err)
	}
	if len(cp.WaitingDeposits[102]) != 1 || mappedDeposits(cp, deps) != 0 || cp.LastScannedBlock != 102 {
		t.Fatalf("setup: want the deposit parked at 102 and unmapped, scan at 102; got parked %d, mapped %d, scan %d",
			len(cp.WaitingDeposits[102]), mappedDeposits(cp, deps), cp.LastScannedBlock)
	}

	// The chain moves far ahead and the deposit's header is stored.
	l1.block(5100)
	l2.set("vsc1verifier", "h", "5100")
	rpc.client.Transport = &slowRPC{delay: 20 * time.Millisecond}

	// Tick 2: a 100-block scan backlog on a slow RPC. The parked deposit must
	// still be mapped in this tick.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel2()
	if err := runOnce(ctx2, rpc, gql, sub, cp, cfg, "vsc1bridge-primary"); err != nil {
		t.Fatal(err)
	}
	if n := mappedDeposits(cp, deps); n != 1 {
		t.Fatalf("the parked deposit (header stored) was not mapped while the scan was behind: mapped %d/1, scan at %d of 5100",
			n, cp.LastScannedBlock)
	}
	if cp.LastScannedBlock <= 102 {
		t.Fatalf("the scan made no progress in the tick: at %d", cp.LastScannedBlock)
	}
}

// The first RESCAN-STARVE-1 fix gave the scan a fixed half of the deposit
// phase. With nothing parked that only halved how fast a bot behind catches
// up: on the devnet BB2 rerun the scan then did ~45 blocks/min against a chain
// at ~67, never reached the stored L1 height, and retireVault waited forever
// (I-26). With nothing parked the scan must keep the whole phase: on a slow RPC
// it scans clearly more blocks than half the phase allows.
func TestRESCANSTARVE1_ScanKeepsTheWholeDepositPhaseWhenNothingIsParked(t *testing.T) {
	const budget = 1200 * time.Millisecond
	withDepositBudget(t, budget)
	slow := &slowRPC{delay: 40 * time.Millisecond}
	l1, l2, rpc, gql, cp, cfg, sub, _ := tickWorld(t, 101, slow)
	l1.block(5100)
	l2.set("vsc1verifier", "h", "5100")
	half := func() uint64 { // blocks one tick scans in budget/2 on this RPC
		calls0, t0 := slow.calls.Load(), time.Now()
		w := &Checkpoint{LastScannedBlock: 1000}
		ctx, cancel := context.WithTimeout(context.Background(), budget/2)
		defer cancel()
		res := scanBlocksParallel(ctx, rpc, w.LastScannedBlock+1, w.LastScannedBlock+100, cfg)
		n := uint64(0)
		for h := uint64(1001); h <= 1100; h++ {
			if r, ok := res[h]; !ok || r.err != nil {
				break
			}
			n++
		}
		t.Logf("calibration: %d blocks in %s (%d RPC calls)", n, time.Since(t0).Round(10*time.Millisecond), slow.calls.Load()-calls0)
		return n
	}()
	start := cp.LastScannedBlock
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	if err := runOnce(ctx, rpc, gql, sub, cp, cfg, "vsc1bridge-primary"); err != nil {
		t.Fatal(err)
	}
	got := cp.LastScannedBlock - start
	t.Logf("one tick scanned %d blocks; half the phase allows %d", got, half)
	if half >= 100 {
		t.Skipf("RPC too fast to tell (half the phase scans the whole 100-block window)")
	}
	if got <= half+half/4 {
		t.Fatalf("one tick scanned %d blocks with nothing parked, about what half the deposit phase allows (%d): the scan is capped", got, half)
	}
}

// slowRefuseSubmits makes every L2 submission take delay and then fail.
type slowRefuseSubmits struct{ delay time.Duration }

func (s slowRefuseSubmits) RoundTrip(r *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(r.Body)
	if strings.Contains(string(body), "submitTransactionV1") {
		select {
		case <-time.After(s.delay):
		case <-r.Context().Done():
		}
		return nil, errors.New("submission refused")
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return http.DefaultTransport.RoundTrip(r)
}

// The other way round: maps go first, so a parked block whose maps keep
// failing (it is retried every tick, never dead-lettered) must not take the
// scan's time. Here every submission is slow and refused, three deposits are
// parked with their header stored, and the scan is far behind: the scan still
// gets about half the deposit phase every tick.
func TestRESCANSTARVE1_FailingParkedMapsDoNotHoldTheScan(t *testing.T) {
	const budget = 1200 * time.Millisecond
	withDepositBudget(t, budget)
	slow := &slowRPC{delay: 40 * time.Millisecond}
	l1, l2, rpc, gql, cp, cfg, sub, vault := tickWorld(t, 101, &slowRPC{delay: time.Millisecond})
	busyBlock(t, l1.block(102), 6, vault, 1, 3, 5)
	delete(cp.SentWithdrawals, "0") // only the deposits submit here
	ctx1, cancel1 := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel1()
	if err := runOnce(ctx1, rpc, gql, sub, cp, cfg, "vsc1bridge-primary"); err != nil {
		t.Fatal(err)
	}
	if len(cp.WaitingDeposits[102]) != 3 {
		t.Fatalf("setup: want 3 deposits parked at 102, got %v", cp.WaitingDeposits)
	}
	l1.block(5100)
	l2.set("vsc1verifier", "h", "5100")
	rpc.client.Transport = slow
	gql.client.Transport = slowRefuseSubmits{delay: 400 * time.Millisecond}
	start := cp.LastScannedBlock
	ctx2, cancel2 := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel2()
	_ = runOnce(ctx2, rpc, gql, sub, cp, cfg, "vsc1bridge-primary")
	got := cp.LastScannedBlock - start
	// half the phase on this RPC scans ~64 blocks (see the test above); the
	// failing maps alone would take 3 deposits x 3 tries x 400 ms + backoff.
	t.Logf("scan advanced %d blocks while the parked maps failed", got)
	if got < 40 {
		t.Fatalf("the scan advanced %d blocks in a tick whose parked maps kept failing: the failing maps held the scan", got)
	}
	if len(cp.WaitingDeposits[102]) != 3 {
		t.Fatalf("the failed deposits must stay parked: %v", cp.WaitingDeposits)
	}
}
