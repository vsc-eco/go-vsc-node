package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

func drainWorld(t *testing.T) (*fakeL2, *l2Submitter) {
	t.Helper()
	l2, sub := poolWorld(t)
	l2.pool = nil // statuses and results come from l2.status and l2.result
	for k, v := range map[string]string{"wqh": "3", "wqt": "4", "n": "10", "np": "10", "zkverifier": "vsc1verifier"} {
		l2.set("vsc1bridge", k, v)
	}
	l2.set("vsc1verifier", "h", "500")
	l2.set("vsc1verifier", "b-500", tipHeader(time.Now().Add(-5*time.Minute)))
	sentDrains, lastDrainAt, drainSkipping = nil, time.Time{}, false
	t.Cleanup(func() { sentDrains, lastDrainAt, drainSkipping = nil, time.Time{}, false })
	return l2, sub
}

// BB-1 (11-06): 65 drains in a row were logged "drained" while the queue did
// not move: the line was written when the pool took the call. The broadcast is
// now logged as sent, and what the call did only once the chain has run it.
func TestRP11_ADrainIsReportedByItsResultNotItsBroadcast(t *testing.T) {
	l2, sub := drainWorld(t)
	logs := captureLogs(t)
	status := "INCLUDED"
	l2.status = func(string) string { return status }
	l2.outputs = func(string) []fakeOutput {
		return []fakeOutput{{"vsc1bridge", map[string]any{"ok": true, "ret": "1", "logs": []string{"unmap|n=10|f=a"}}}}
	}
	ctx, gql := context.Background(), l2.gql(t)

	drainIfQueued(ctx, gql, sub, sub.cfg)
	if calls := submittedCalls(t, l2); len(calls) != 1 || calls[0].Action != "drainWithdrawalQueue" {
		t.Fatalf("calls %+v, want one drain", calls)
	}
	if out := logs.String(); !strings.Contains(out, "RP-11: drainWithdrawalQueue sent") || strings.Contains(out, "drained") || strings.Contains(out, "landed") {
		t.Fatalf("the broadcast must say sent, nothing more:\n%s", out)
	}

	// Still running: nothing to report, and it is asked about again.
	drainIfQueued(ctx, gql, sub, sub.cfg)
	if strings.Contains(logs.String(), "landed") || len(sentDrains) != 1 {
		t.Fatalf("reported a drain still running (%d tracked):\n%s", len(sentDrains), logs.String())
	}

	status = "CONFIRMED"
	drainIfQueued(ctx, gql, sub, sub.cfg)
	out := logs.String()
	if !strings.Contains(out, "RP-11: drain landed") || !strings.Contains(out, "issued=1") || !strings.Contains(out, "refunded=0") ||
		!strings.Contains(out, "queuedBefore=1") || !strings.Contains(out, "inFlightBefore=0") {
		t.Fatalf("result not reported:\n%s", out)
	}
	if len(sentDrains) != 0 {
		t.Fatalf("a reported drain is still tracked: %+v", sentDrains)
	}
	logs.Reset()
	reportDrains(ctx, gql, "vsc1bridge")
	if strings.Contains(logs.String(), "RP-11") {
		t.Fatalf("reported twice:\n%s", logs.String())
	}
}

func TestRP11_DrainResults(t *testing.T) {
	verifierRecord := fakeOutput{"vsc1verifier", map[string]any{"ok": true, "ret": ""}}
	bridgeIssued5 := fakeOutput{"vsc1bridge", map[string]any{"ok": true, "ret": "5", "logs": []string{"unmap|n=16|f=a"}}}
	longAgo := func() time.Time { return time.Now().Add(-confirmVanishGrace - time.Minute) }
	for _, c := range []struct {
		name     string
		status   string       // "-": the node does not know the transaction
		outputs  []fakeOutput // nil: result is the bridge's only output
		result   map[string]any
		errMsg   string
		sentAt   func() time.Time
		want     []string // in the log; empty with done false: nothing yet
		wantNot  []string
		wantDone bool
	}{
		{name: "issued", status: "CONFIRMED", result: map[string]any{"ok": true, "ret": "2"},
			want: []string{"drain landed", "issued=2", "refunded=0"}, wantNot: []string{"moved no"}, wantDone: true},
		{name: "PROCESSED counts as landed", status: "PROCESSED", result: map[string]any{"ok": true, "ret": "1"},
			want: []string{"drain landed", "issued=1"}, wantDone: true},
		{name: "refund only, as QFREEZE-1 leaves it", status: "CONFIRMED",
			result: map[string]any{"ok": true, "ret": "0", "logs": []string{"unmapq|refund|q=24|f=a|a=eth|d=1|why=x", "other|y"}},
			want:   []string{"drain landed", "issued=0", "refunded=1"}, wantNot: []string{"moved no"}, wantDone: true},
		// The verifier's record of a drain that also touched it: always ret "", and
		// the order varies (testnet 11-07: 7 drains each way out of 72; bafyreie6x4..
		// issued 5, verifier's record first).
		{name: "verifier's record first", status: "CONFIRMED", outputs: []fakeOutput{verifierRecord, bridgeIssued5},
			want: []string{"drain landed", "issued=5"}, wantNot: []string{"unreadable"}, wantDone: true},
		{name: "bridge's record first", status: "CONFIRMED", outputs: []fakeOutput{bridgeIssued5, verifierRecord},
			want: []string{"drain landed", "issued=5"}, wantNot: []string{"unreadable"}, wantDone: true},
		{name: "only another contract's record", status: "CONFIRMED", outputs: []fakeOutput{verifierRecord},
			want: []string{"its result is unreadable"}, wantDone: true},
		{name: "failed, the error behind another contract's record", status: "FAILED",
			outputs: []fakeOutput{verifierRecord, {"vsc1bridge", map[string]any{"ok": false, "errMsg": "cost limit exceeded"}}},
			want:    []string{"drain failed on chain", "cost limit exceeded"}, wantDone: true},
		{name: "ran and moved nothing", status: "CONFIRMED", result: map[string]any{"ok": true, "ret": "0"},
			want: []string{"moved no withdrawal", "issued=0", "refunded=0"}, wantDone: true},
		{name: "no result recorded", status: "CONFIRMED", result: map[string]any{"ok": true},
			want: []string{"its result is unreadable"}, wantNot: []string{"drain landed\""}, wantDone: true},
		{name: "failed on chain", status: "FAILED", errMsg: "cost limit exceeded",
			want: []string{"drain failed on chain", "cost limit exceeded"}, wantNot: []string{"landed"}, wantDone: true},
		{name: "not known yet", status: "-"},
		{name: "not known long after", status: "-", sentAt: longAgo,
			want: []string{"drain has no result", "transaction not found"}, wantDone: true},
		{name: "still pending long after", status: "INCLUDED", sentAt: longAgo,
			want: []string{"drain has no result", "status=INCLUDED"}, wantDone: true},
		{name: "landed, result not readable yet", status: "CONFIRMED", result: map[string]any{"ret": 5}},
		{name: "landed, result never readable", status: "CONFIRMED", result: map[string]any{"ret": 5}, sentAt: longAgo,
			want: []string{"its result is unreadable", "decode output"}, wantDone: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			l2, _ := drainWorld(t)
			logs := captureLogs(t)
			l2.status = func(string) string { return c.status }
			if c.errMsg != "" {
				l2.errMsg = func(string) string { return c.errMsg }
			}
			if c.result != nil {
				c.outputs = []fakeOutput{{"vsc1bridge", c.result}}
			}
			if c.outputs != nil {
				l2.outputs = func(string) []fakeOutput { return c.outputs }
			}
			at := time.Now()
			if c.sentAt != nil {
				at = c.sentAt()
			}
			sentDrains = []sentDrain{{tx: "tx-1", queued: 1, at: at}}
			reportDrains(context.Background(), l2.gql(t), "vsc1bridge")
			out := logs.String()
			if done := len(sentDrains) == 0; done != c.wantDone {
				t.Fatalf("done %v, want %v:\n%s", done, c.wantDone, out)
			}
			if !c.wantDone && strings.Contains(out, "RP-11") {
				t.Fatalf("reported before there was a result:\n%s", out)
			}
			for _, w := range c.want {
				if !strings.Contains(out, w) {
					t.Fatalf("log lacks %q:\n%s", w, out)
				}
			}
			for _, w := range c.wantNot {
				if strings.Contains(out, w) {
					t.Fatalf("log has %q:\n%s", w, out)
				}
			}
		})
	}
}

// Two drains unanswered at once: each is reported, neither hides the other.
func TestRP11_EachUnansweredDrainIsReported(t *testing.T) {
	l2, _ := drainWorld(t)
	logs := captureLogs(t)
	l2.status = func(id string) string {
		if id == "tx-1" {
			return "CONFIRMED"
		}
		return "INCLUDED"
	}
	l2.outputs = func(string) []fakeOutput { return []fakeOutput{{"vsc1bridge", map[string]any{"ret": "1"}}} }
	sentDrains = []sentDrain{{tx: "tx-1", at: time.Now()}, {tx: "tx-2", at: time.Now()}}
	reportDrains(context.Background(), l2.gql(t), "vsc1bridge")
	if len(sentDrains) != 1 || sentDrains[0].tx != "tx-2" || !strings.Contains(logs.String(), "tx=tx-1") {
		t.Fatalf("tracked %+v:\n%s", sentDrains, logs.String())
	}
}
