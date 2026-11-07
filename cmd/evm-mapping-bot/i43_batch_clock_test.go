package main

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// I-43 (testnet 11-05: 3 of 24 proofs proved at once): the needs file's "since"
// (the operator's batch clock) restarts when the proven tip moves with a need
// still open, instead of carrying a clock that already ran out; while the tip
// stays, it keeps counting. The A10 alarm's clock (WantProofSince) keeps
// counting across proofs, so a need every proof misses still alarms.
func TestI43_BatchClockRestartsWhenTheTipMoves(t *testing.T) {
	l2 := newFakeL2()
	l2.set("vsc1bridge", "zkverifier", "vsc1verifier")
	gql := l2.gql(t)
	path := t.TempDir() + "/needs.json"
	t.Setenv("NEEDS_FILE", path)
	cfg := EVMBotConfig{ContractID: "vsc1bridge"}
	since := func() int64 {
		var m struct {
			Since int64 `json:"since"`
		}
		b, _ := os.ReadFile(path)
		_ = json.Unmarshal(b, &m)
		return m.Since
	}
	cp := &Checkpoint{HeaderNeeds: map[uint64]int64{150: time.Now().Unix()}}
	writeNeedsFile(context.Background(), gql, cp, cfg, 140, false)
	if since() == 0 || cp.WantProofSince == 0 {
		t.Fatalf("a need above the tip did not start the clocks: since %d, alarm %d", since(), cp.WantProofSince)
	}
	// 11 minutes pass with the need open at the same tip: the batch is due.
	old := time.Now().Add(-11 * time.Minute).Unix()
	cp.WantBatchSince, cp.WantProofSince = old, old
	writeNeedsFile(context.Background(), gql, cp, cfg, 140, false)
	if since() != old {
		t.Fatalf("same tip: since %d, want the batch's start %d (it keeps counting)", since(), old)
	}
	// A proof lands at 145, below the need: a new batch starts; the alarm clock keeps.
	writeNeedsFile(context.Background(), gql, cp, cfg, 145, false)
	if s := since(); s <= old || time.Now().Unix()-s > 5 {
		t.Fatalf("tip moved with the need still open: since %d, want now (was %d)", s, old)
	}
	if cp.WantProofSince != old {
		t.Fatalf("the A10 clock restarted with the batch: %d, want %d", cp.WantProofSince, old)
	}
	// The need is met: both clocks stop.
	cp.HeaderNeeds = map[uint64]int64{}
	writeNeedsFile(context.Background(), gql, cp, cfg, 160, false)
	if cp.WantProofSince != 0 || cp.WantBatchSince != 0 {
		t.Fatalf("clocks not cleared when nothing is wanted: alarm %d, batch %d", cp.WantProofSince, cp.WantBatchSince)
	}
}
