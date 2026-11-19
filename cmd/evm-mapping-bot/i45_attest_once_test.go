package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// i45World is a verifier at tip 1000 with the given roots stored and every
// needed header open, and an attestation cache holding, per root, the needs it
// covers (the live shape: each new proof's prefetch cached the needs open then).
func i45World(t *testing.T, cache map[uint64][]uint64, needs []uint64) (*fakeL2, *l2Submitter, *Checkpoint, *ethRPC) {
	t.Helper()
	l1 := newFakeL1()
	l1.block(1000)
	l2, sub := poolWorld(t)
	l2.set("vsc1bridge", "zkverifier", "vsc1verifier")
	l2.set("vsc1verifier", "h", "1000")
	l2.onDemand = true
	t.Setenv("NEEDS_FILE", t.TempDir()+"/needs.json")
	cp := &Checkpoint{HeaderNeeds: map[uint64]int64{}, ProofWindow: 1, AttestCache: map[uint64]*attestEntry{}}
	for _, x := range needs {
		cp.HeaderNeeds[x] = time.Now().Unix()
	}
	for f, xs := range cache {
		l2.set("vsc1verifier", fmt.Sprintf("b-%d", f), strings.Repeat("\x00", 137))
		e := &attestEntry{AccountProof: []string{"0x02"}, Headers: map[uint64]attestedBlock{}}
		for _, x := range xs {
			e.Headers[x] = attestedBlock{RLP: fmt.Sprintf("%08x", x), StorageProof: []string{"0x01"}}
		}
		cp.AttestCache[f] = e
	}
	return l2, sub, cp, l1.rpc(t)
}

// attestedHeaders tallies the headers in the submitted attestHeaders calls, by
// block, and the roots they went from.
func attestedHeaders(t *testing.T, l2 *fakeL2) (map[uint64]int, map[uint64]int) {
	t.Helper()
	byBlock, byRoot := map[uint64]int{}, map[uint64]int{}
	for _, c := range submittedCalls(t, l2) {
		if c.Action != "attestHeaders" {
			continue
		}
		var p struct {
			Root    uint64 `json:"root_block"`
			Headers []struct {
				RLP string `json:"rlp_hex"`
			} `json:"headers"`
		}
		if err := json.Unmarshal([]byte(c.Payload), &p); err != nil {
			t.Fatal(err)
		}
		for _, h := range p.Headers {
			var x uint64
			fmt.Sscanf(h.RLP, "%x", &x)
			byBlock[x]++
			byRoot[p.Root]++
		}
	}
	return byBlock, byRoot
}

// I-45 (testnet 11-05 16:52): each needed header is attested from one root,
// the newest that holds it, not once per cached root (six roots attested 11
// headers 37 times in 21 calls, each declaring the rc_limit of fresh headers,
// which the block producer reserves to include it).
func TestI45_EachNeededHeaderIsAttestedFromOneRoot(t *testing.T) {
	needs := []uint64{800, 810, 900, 950}
	l2, sub, cp, rpc := i45World(t, map[uint64][]uint64{
		1000: {800, 810, 900, 950},
		960:  {800, 810, 900},
		930:  {800, 810},
	}, needs)
	handleHeaderNeeds(context.Background(), rpc, l2.gql(t), sub, cp, sub.cfg)
	byBlock, byRoot := attestedHeaders(t, l2)
	for _, x := range needs {
		if byBlock[x] != 1 {
			t.Fatalf("block %d attested %d times, want once (all: %v)", x, byBlock[x], byBlock)
		}
	}
	if byRoot[1000] != 4 || len(byRoot) != 1 {
		t.Fatalf("headers by root %v, want all 4 from the newest root 1000", byRoot)
	}
	roots := 0
	for _, c := range submittedCalls(t, l2) {
		if c.Action == "attestHistoryRoot" {
			roots++
		}
	}
	if roots != 1 {
		t.Fatalf("%d attestHistoryRoot calls, want 1 (the older roots attest nothing)", roots)
	}
}

// I-45: the tick's attestations stop at the bot's RC. The node admits each call
// against the bot's RC alone, so calls past it were accepted and then waited,
// holding every later nonce (live: 14 calls, ~4 h, ahead of the maps). When RC
// covers none, nothing goes and the root tries again after attestResendAfter
// (not every 12 s tick: no dry runs meanwhile); the rest go on a later tick
// once the first are stored.
func TestI45_AttestationsStopAtTheBotsRC(t *testing.T) {
	needs := []uint64{800, 810, 820, 830, 840, 850}
	for _, c := range []struct {
		rc        int64
		wantCalls int
	}{{5_000, 0}, {13_000, 2}, {1_000_000, 3}} {
		l2, sub, cp, rpc := i45World(t, map[uint64][]uint64{1000: needs}, needs)
		l2.set("vsc1verifier", "hr-1000", strings.Repeat("r", 32)) // the root is stored: headers only
		dryRuns := 0
		l2.sim = func(action string) map[string]any {
			dryRuns++
			return map[string]any{"success": true, "ret": "", "rc_used": int64(3_000)} // 6,000 declared a call
		}
		l2.rc = map[string]int64{sub.did.String(): c.rc}
		handleHeaderNeeds(context.Background(), rpc, l2.gql(t), sub, cp, sub.cfg)
		calls := submittedCalls(t, l2)
		if len(calls) != c.wantCalls {
			t.Fatalf("RC %d: %d calls %+v, want %d attestHeaders of 6,000", c.rc, len(calls), calls, c.wantCalls)
		}
		if _, marked := cp.AttestSentAt[1000]; !marked {
			t.Fatalf("RC %d: the attempt is not marked", c.rc)
		}
		if c.rc == 5_000 {
			before := dryRuns
			handleHeaderNeeds(context.Background(), rpc, l2.gql(t), sub, cp, sub.cfg) // the next tick, RC still short
			if dryRuns != before || len(submittedCalls(t, l2)) != 0 {
				t.Fatalf("RC 5,000: the next tick ran %d dry runs and %d calls; a root short of RC retries after %s, not every tick",
					dryRuns-before, len(submittedCalls(t, l2)), attestResendAfter)
			}
		}
		if c.rc != 13_000 {
			continue
		}
		// The two calls executed; the next tick (after the resend window) sends the rest.
		byBlock, _ := attestedHeaders(t, l2)
		for x := range byBlock {
			l2.set("vsc1verifier", fmt.Sprintf("b-%d", x), strings.Repeat("\x00", 137))
		}
		cp.AttestSentAt[1000] = time.Now().Add(-attestResendAfter - time.Second).Unix()
		handleHeaderNeeds(context.Background(), rpc, l2.gql(t), sub, cp, sub.cfg)
		byBlock, _ = attestedHeaders(t, l2)
		for _, x := range needs {
			if byBlock[x] != 1 {
				t.Fatalf("after the second tick block %d attested %d times, want once (%v)", x, byBlock[x], byBlock)
			}
		}
	}
}

// I-45: a root that sends nothing for its headers leaves them to an older root,
// as every root used to try them. Here the newest root (1000) still needs its
// own attestHistoryRoot and the bot's RC covers only the headers call, which
// the older root (960, its history root stored) can send alone.
func TestI45_ARootThatSendsNothingLeavesItsHeadersToAnOlderOne(t *testing.T) {
	l2, sub, cp, rpc := i45World(t, map[uint64][]uint64{1000: {800}, 960: {800}}, []uint64{800})
	l2.set("vsc1verifier", "hr-960", strings.Repeat("r", 32))
	l2.sim = func(action string) map[string]any {
		return map[string]any{"success": true, "ret": "", "rc_used": int64(3_000)} // 6,000 declared a call
	}
	l2.rc = map[string]int64{sub.did.String(): 7_000} // the headers call, not the root's call + the headers call
	handleHeaderNeeds(context.Background(), rpc, l2.gql(t), sub, cp, sub.cfg)
	calls := submittedCalls(t, l2)
	if len(calls) != 1 || calls[0].Action != "attestHeaders" {
		t.Fatalf("calls %+v, want one attestHeaders from the older root", calls)
	}
	if _, byRoot := attestedHeaders(t, l2); byRoot[960] != 1 {
		t.Fatalf("headers by root %v, want block 800 from root 960", byRoot)
	}
}

// I-45: while a newer root's attestation may still be pending (inside
// attestResendAfter), an older root does not send the same header on a later
// tick.
func TestI45_APendingAttestationHoldsItsHeadersOnTheNextTick(t *testing.T) {
	l2, sub, cp, rpc := i45World(t, map[uint64][]uint64{1000: {800, 810}, 960: {800, 810}}, []uint64{800, 810})
	handleHeaderNeeds(context.Background(), rpc, l2.gql(t), sub, cp, sub.cfg)
	first := len(submittedCalls(t, l2))
	if _, byRoot := attestedHeaders(t, l2); byRoot[1000] != 2 || byRoot[960] != 0 {
		t.Fatalf("first tick: headers by root %v, want both from 1000", byRoot)
	}
	handleHeaderNeeds(context.Background(), rpc, l2.gql(t), sub, cp, sub.cfg) // not executed yet
	if got := len(submittedCalls(t, l2)); got != first {
		_, byRoot := attestedHeaders(t, l2)
		t.Fatalf("second tick sent %d more calls (headers by root %v) while the first may still be pending", got-first, byRoot)
	}
}
