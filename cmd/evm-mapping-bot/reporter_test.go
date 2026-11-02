package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"vsc-node/lib/btcvault"
)

// fakeReporter answers a round: per action, what the dry run says; and
// records what is submitted.
type fakeReporter struct {
	gens      []vaultGen
	theft     map[uint32]bool // reportUnauthorizedSpend accepted for gen
	custody   string          // last proveCustody result ("custody held"/"custody short"/"" refused)
	flagTheft bool
	flagShort bool
	proofErr  error
	submitted []l2Call
}

func (f *fakeReporter) deps() reporterDeps {
	return reporterDeps{
		gens:   func() ([]vaultGen, error) { return f.gens, nil },
		header: func() (uint64, error) { return 500, nil },
		proof: func(addr string, h uint64) (accountProof, error) {
			if f.proofErr != nil {
				return accountProof{}, f.proofErr
			}
			return accountProof{BlockHeight: h, Proof: addr}, nil
		},
		flags: func() (bool, bool, error) { return f.flagTheft, f.flagShort, nil },
		simulate: func(calls []l2Call) ([]simulatedCall, error) {
			out := make([]simulatedCall, len(calls))
			for i, c := range calls {
				var p struct {
					Gen uint32 `json:"gen"`
				}
				_ = json.Unmarshal(c.Payload, &p)
				switch c.Action {
				case "reportUnauthorizedSpend":
					out[i] = simulatedCall{Success: f.theft[p.Gen], RcUsed: 620, ErrMsg: "no unauthorized spend"}
				case "proveCustody":
					out[i] = simulatedCall{Success: true, RcUsed: 532, Ret: "custody proof at block 500: 1 of 2 generations"}
					if i == len(calls)-1 {
						out[i] = simulatedCall{Success: f.custody != "", RcUsed: 532, Ret: f.custody + " at block 500", ErrMsg: "being assembled"}
					}
				}
			}
			return out, nil
		},
		submit: func(c l2Call, _ int64) error { f.submitted = append(f.submitted, c); return nil },
	}
}

func twoGens() []vaultGen {
	return []vaultGen{{Gen: 1, Status: btcvault.VaultStatusActive, Address: "0xa1"}, {Gen: 0, Status: btcvault.VaultStatusRetiring, Address: "0xb0"}}
}

// B6: a healthy bridge costs nothing; a theft or a custody change is sent.
func TestReporter_SendsOnlyWhatChangesSomething(t *testing.T) {
	f := &fakeReporter{gens: twoGens(), custody: "custody held"}
	if out, err := runReporter(f.deps()); err != nil || len(f.submitted) != 0 || !strings.HasPrefix(out, "nothing to report") {
		t.Fatalf("healthy: %q %v %v", out, err, f.submitted)
	}

	f = &fakeReporter{gens: twoGens(), custody: "custody held", theft: map[uint32]bool{0: true}}
	if out, err := runReporter(f.deps()); err != nil || len(f.submitted) != 1 || f.submitted[0].Action != "reportUnauthorizedSpend" ||
		!strings.Contains(string(f.submitted[0].Payload), `"gen":0`) || !strings.HasPrefix(out, "theft reported") {
		t.Fatalf("theft at gen 0: %q %v %v", out, err, f.submitted)
	}
	f = &fakeReporter{gens: twoGens(), custody: "custody held", theft: map[uint32]bool{0: true}, flagTheft: true}
	if _, err := runReporter(f.deps()); err != nil || len(f.submitted) != 0 {
		t.Fatalf("theft already halted: reported again %v", f.submitted)
	}

	f = &fakeReporter{gens: twoGens(), custody: "custody short"}
	out, err := runReporter(f.deps())
	if err != nil || len(f.submitted) != 2 || !strings.HasPrefix(out, "custody short reported") {
		t.Fatalf("short: %q %v %v", out, err, f.submitted)
	}
	for i, want := range []string{`"gen":1`, `"gen":0`} { // every generation, in view order
		if f.submitted[i].Action != "proveCustody" || !strings.Contains(string(f.submitted[i].Payload), want) {
			t.Fatalf("submission %d %s %s", i, f.submitted[i].Action, f.submitted[i].Payload)
		}
	}
	f = &fakeReporter{gens: twoGens(), custody: "custody short", flagShort: true}
	if _, err := runReporter(f.deps()); err != nil || len(f.submitted) != 0 {
		t.Fatalf("short and already halted: sent %v", f.submitted)
	}
	f = &fakeReporter{gens: twoGens(), custody: "custody held", flagShort: true}
	if out, err := runReporter(f.deps()); err != nil || len(f.submitted) != 2 || !strings.HasPrefix(out, "custody restored reported") {
		t.Fatalf("held while halted (lift): %q %v %v", out, err, f.submitted)
	}
}

// A round that cannot prove (pruned RPC) or whose proof the bridge would not
// take now sends nothing.
func TestReporter_SkipsWhatItCannotProve(t *testing.T) {
	f := &fakeReporter{gens: twoGens(), custody: "custody short", proofErr: errors.New("missing trie node")}
	if _, err := runReporter(f.deps()); err == nil || len(f.submitted) != 0 {
		t.Fatalf("eth_getProof failed: err %v sent %v", err, f.submitted)
	}
	f = &fakeReporter{gens: twoGens(), custody: ""} // refused (another proof being assembled)
	if out, err := runReporter(f.deps()); err != nil || len(f.submitted) != 0 || !strings.Contains(out, "not accepted now") {
		t.Fatalf("refused dry run: %q %v %v", out, err, f.submitted)
	}
	f = &fakeReporter{}
	if out, _ := runReporter(f.deps()); out != "no fund-holding generation" {
		t.Fatalf("no generations: %q", out)
	}
}

// A pre-rotation bridge's one vault is generation 0; a rotated bridge lists
// its fund-holding generations with an address.
func TestReporter_FundHoldingGens(t *testing.T) {
	if g := fundHoldingGens(vaultView{}, EVMBotConfig{VaultAddress: "0xABC"}); len(g) != 1 || g[0].Gen != 0 || g[0].Address != "0xabc" {
		t.Fatalf("pre-rotation: %+v", g)
	}
	view := vaultView{Registry: true, Gens: []vaultGen{
		{Gen: 0, Status: btcvault.VaultStatusPurged, Address: "0x0"},
		{Gen: 1, Status: btcvault.VaultStatusInactive, Address: "0x1"},
		{Gen: 2, Status: btcvault.VaultStatusActive, Address: "0x2"},
		{Gen: 3, Status: btcvault.VaultStatusPending},
	}}
	g := fundHoldingGens(view, EVMBotConfig{})
	if len(g) != 2 || g[0].Gen != 1 || g[1].Gen != 2 {
		t.Fatalf("rotated: %+v", g)
	}
}

func TestReporter_Interval(t *testing.T) {
	old, oldNow, oldLast := reporterEvery, reporterNow, lastReportAt
	t.Cleanup(func() { reporterEvery, reporterNow, lastReportAt = old, oldNow, oldLast })
	now := time.Unix(1_000_000, 0)
	reporterNow = func() time.Time { return now }
	reporterEvery, lastReportAt = 10*time.Minute, time.Time{}
	if !reporterDue() || reporterDue() {
		t.Fatal("first round must run, an immediate second must not")
	}
	now = now.Add(10 * time.Minute)
	if !reporterDue() {
		t.Fatal("a round after the interval did not run")
	}
	reporterEvery = 0
	now = now.Add(time.Hour)
	if reporterDue() {
		t.Fatal("REPORTER_EVERY=0 must turn the reporter off")
	}
}
