package tss

import (
	"context"
	"testing"
	"time"

	systemconfig "vsc-node/modules/common/system-config"

	btss "github.com/bnb-chain/tss-lib/v3/tss"
)

func TestTimeoutDecision(t *testing.T) {
	const T = 10 * time.Second
	hold := maxComputeHold
	cases := []struct {
		name       string
		since      time.Duration
		processing bool
		fire       bool
		wait       time.Duration
	}{
		{"recent activity", 4 * time.Second, false, false, 6 * time.Second},
		{"recent activity, processing", 4 * time.Second, true, false, 6 * time.Second},
		{"quiet for the timeout", T, false, true, 0},
		{"quiet but processing", T + time.Second, true, false, timeoutRecheck},
		{"processing near the cap", T + hold - 300*time.Millisecond, true, false, 300 * time.Millisecond},
		{"processing past the cap", T + hold, true, true, 0},
	}
	for _, c := range cases {
		fire, wait := timeoutDecision(c.since, T, c.processing)
		if fire != c.fire || wait != c.wait {
			t.Errorf("%s: timeoutDecision(%s, %s, %v) = (%v, %s), want (%v, %s)", c.name, c.since, T, c.processing, fire, wait, c.fire, c.wait)
		}
	}
}

func TestExtendTimeoutWhileProcessingOffOnMainnet(t *testing.T) {
	if extendTimeoutWhileProcessing(true) {
		t.Fatal("mainnet must keep its timeout unchanged")
	}
	if !extendTimeoutWhileProcessing(false) {
		t.Fatal("off mainnet a message in progress must hold the timeout off")
	}
}

// monitorDispatcher is a dispatcher with just what baseStart's timeout monitor
// touches, on the given network with the given session timeout.
func monitorDispatcher(t *testing.T, base systemconfig.SystemConfig, timeout time.Duration) *BaseDispatcher {
	t.Helper()
	tp := base.TssParams()
	tp.DefaultTimeout = timeout
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	d := &BaseDispatcher{
		tssMgr:     &TssManager{sconf: shortPreParamsConfig{SystemConfig: base, tp: tp}},
		msgCtx:     ctx,
		cancelMsgs: cancel,
		done:       make(chan struct{}),
		p2pMsg:     make(chan btss.Message, 1),
	}
	d.startLock.Lock() // baseStart releases it
	return d
}

// timedOutAfter starts the monitor and returns how long it took to time the
// session out, or ok=false if it had not within limit.
func timedOutAfter(d *BaseDispatcher, limit time.Duration, during func()) (time.Duration, bool) {
	start := time.Now()
	d.baseStart()
	if during != nil {
		go during()
	}
	select {
	case <-d.done:
		return time.Since(start), true
	case <-time.After(limit):
		return 0, false
	}
}

const monitorT = 300 * time.Millisecond

func TestTimeoutMonitorIdleSessionTimesOutAtTheTimeout(t *testing.T) {
	for name, base := range map[string]systemconfig.SystemConfig{"mainnet": systemconfig.MainnetConfig(), "devnet": systemconfig.DevnetConfig()} {
		d := monitorDispatcher(t, base, monitorT)
		took, ok := timedOutAfter(d, 5*monitorT, nil)
		if !ok || took < monitorT || took > 2*monitorT {
			t.Errorf("%s: idle session timed out after %s (ok=%v), want about %s", name, took, ok, monitorT)
		}
	}
}

// A slow node computing a round holds a call in progress past the timeout; it
// must not time itself out, and must once the call returns without activity.
func TestTimeoutMonitorWaitsForAMessageInProgress(t *testing.T) {
	d := monitorDispatcher(t, systemconfig.DevnetConfig(), monitorT)
	d.processing.Add(1)
	took, ok := timedOutAfter(d, 5*monitorT, func() {
		time.Sleep(3 * monitorT / 2) // computing for 1.5x the timeout
		d.processing.Add(-1)
	})
	if !ok || took < 3*monitorT/2 {
		t.Fatalf("timed out after %s (ok=%v) while a message was still in progress at %s", took, ok, 3*monitorT/2)
	}
	if limit := 3*monitorT/2 + timeoutRecheck + monitorT/2; took > limit {
		t.Fatalf("timed out only after %s; once the call returned it should have at the next re-check (by %s)", took, limit)
	}
}

// A call that never returns holds the timeout off only for maxComputeHold.
func TestTimeoutMonitorCapsTheExtension(t *testing.T) {
	old := maxComputeHold
	maxComputeHold = monitorT
	t.Cleanup(func() { maxComputeHold = old })
	d := monitorDispatcher(t, systemconfig.DevnetConfig(), monitorT)
	d.processing.Add(1)
	took, ok := timedOutAfter(d, 6*monitorT, nil)
	if !ok || took < 2*monitorT || took > 3*monitorT {
		t.Fatalf("stuck call: timed out after %s (ok=%v), want about %s", took, ok, 2*monitorT)
	}
}

// Mainnet keeps its timeout exactly: a call in progress changes nothing.
func TestTimeoutMonitorUnchangedOnMainnet(t *testing.T) {
	d := monitorDispatcher(t, systemconfig.MainnetConfig(), monitorT)
	d.processing.Add(1)
	took, ok := timedOutAfter(d, 5*monitorT, nil)
	if !ok || took < monitorT || took > 2*monitorT {
		t.Fatalf("mainnet: timed out after %s (ok=%v) with a call in progress, want about %s as before", took, ok, monitorT)
	}
}
