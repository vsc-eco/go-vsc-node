package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// HEADERNEED-STARVE (11-13 BTC-vs-EVM review): handleHeaderNeeds runs
// last in runOnce on the tick's shared context. If the withdrawal and
// confirmation steps use the rest of the tick (slow RPC), does the operator still
// learn which headers are needed (the needs file)? A header needed above the
// proven tip must reach the needs file in the same tick.
func TestHEADERNEEDSTARVE_NeedsFileWrittenWhenConfirmationsAreSlow(t *testing.T) {
	needs := filepath.Join(t.TempDir(), "needs.json")
	t.Setenv("NEEDS_FILE", needs)
	old := withdrawConfirmBudget
	withdrawConfirmBudget = 800 * time.Millisecond
	t.Cleanup(func() { withdrawConfirmBudget = old })
	slow := &slowRPC{delay: 400 * time.Millisecond}
	_, _, rpc, gql, cp, cfg, sub, _ := tickWorld(t, 101, slow)
	cp.mu.Lock()
	cp.HeaderNeeds = map[uint64]int64{150: time.Now().Unix()}
	cp.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	t0 := time.Now()
	_ = runOnce(ctx, rpc, gql, sub, cp, cfg, "vsc1bridge-primary")
	t.Logf("tick took %s; RPC calls %d", time.Since(t0).Round(10*time.Millisecond), slow.calls.Load())

	b, err := os.ReadFile(needs)
	if err != nil {
		t.Fatalf("needs file not written in the tick (header 150 needed above the proven tip 101): %v", err)
	}
	var got struct {
		WantProof bool     `json:"want_proof"`
		Blocks    []uint64 `json:"blocks"`
	}
	_ = json.Unmarshal(b, &got)
	if !got.WantProof || len(got.Blocks) == 0 || got.Blocks[0] != 150 {
		t.Fatalf("needs file %s does not ask for block 150", b)
	}
}
