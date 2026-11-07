package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// proofServer answers eth_getProof with body (after the HTTP status), every
// other method with block 1000.
func proofServer(t *testing.T, status int, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method != "eth_getProof" {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"0x3e8"}}`))
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

const proofWindowErr = `{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"distance to target block exceeds maximum proof window"}}`
const proofOK = `{"jsonrpc":"2.0","id":1,"result":{"accountProof":["0x01"],"storageProof":[{"proof":["0x02"]}]}}`

// I-5, testnet 11-04 03:23: publicnode answers eth_getProof at every finalized
// block with -32602 "exceeds maximum proof window" (its window is under
// Sepolia's ~70-block finality lag). B14 took that answer as final, and once
// failover had moved the bot onto publicnode every custody report and header
// attestation was skipped. That answer now moves the call to the next
// endpoint, and later calls start where the proof was served.
func TestI5_ProofCapabilityErrorFailsOver(t *testing.T) {
	limited, good := proofServer(t, 200, proofWindowErr), proofServer(t, 200, proofOK)
	rpc := newEthRPC(limited + "," + good)
	res, err := rpc.callCtx(context.Background(), "eth_getProof", `"0x00", [], "0x3e8"`)
	if err != nil || !strings.Contains(string(res), "accountProof") {
		t.Fatalf("eth_getProof: %s %v, want the proof from the second endpoint", res, err)
	}
	if rpc.start.Load() != 1 {
		t.Fatalf("calls start at endpoint %d, want 1 (the one that served the proof)", rpc.start.Load())
	}
	// Every endpoint says it cannot: that is the answer.
	rpc = newEthRPC(limited + "," + limited)
	if _, err := rpc.callCtx(context.Background(), "eth_getProof", `"0x00", [], "0x3e8"`); !isProofCapabilityErr(err) || !isRPCAnswer(err) {
		t.Fatalf("all limited: err %v, want the capability answer", err)
	}
	// One cannot, one does not answer (429): not an answer, ask again later.
	rpc = newEthRPC(limited + "," + proofServer(t, 429, "slow down"))
	if _, err := rpc.callCtx(context.Background(), "eth_getProof", `"0x00", [], "0x3e8"`); err == nil || isRPCAnswer(err) {
		t.Fatalf("limited + 429: err %v, want a no-answer error", err)
	}
	// Any other JSON-RPC error stays final (B14), also for eth_getProof.
	bad := proofServer(t, 200, `{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"invalid argument 1: hex string has odd length"}}`)
	rpc = newEthRPC(bad + "," + good)
	if _, err := rpc.callCtx(context.Background(), "eth_getProof", `"0x00", [], "0x3e8"`); err == nil || !strings.Contains(err.Error(), "odd length") {
		t.Fatalf("a malformed request: err %v, want its error, not a failover", err)
	}
}

// I-18, testnet 11-04 07:00:03 (and 07:46, 10:25, 11:16): one HTTP 429 (or a
// dropped connection) on the attestation prefetch was cached as "this RPC
// serves no state proofs" for the whole finalized block, and every needed
// header went to the submitHeaders link-down (270,000 RC for 9 runs; the bot
// had 85,146). Now nothing is cached and the proof window stays as it was.
func TestI18_TransientProofFailureIsNotCached(t *testing.T) {
	if e := prefetchHistoryProofs(context.Background(), newEthRPC(proofServer(t, 429, "slow down")), 1000, []uint64{700}); e != nil {
		t.Fatalf("429: cached %+v, want nothing cached (ask again next tick)", e)
	}
	if e := prefetchHistoryProofs(context.Background(), newEthRPC(proofServer(t, 200, proofWindowErr)), 1000, []uint64{700}); e == nil || e.Err == "" {
		t.Fatalf("every endpoint answers it cannot serve the proof: %+v, want the failure cached", e)
	}

	l2, sub := poolWorld(t)
	l2.set("vsc1bridge", "zkverifier", "vsc1verifier")
	l2.set("vsc1verifier", "h", "900")
	l2.onDemand = true
	path := t.TempDir() + "/needs.json"
	t.Setenv("NEEDS_FILE", path)
	cp := &Checkpoint{HeaderNeeds: map[uint64]int64{700: time.Now().Unix()}, ProofWindow: 1}
	handleHeaderNeeds(context.Background(), newEthRPC(proofServer(t, 429, "slow down")), l2.gql(t), sub, cp, sub.cfg)
	if cp.ProofWindow != 1 || cp.AttestCache[1000] != nil {
		t.Fatalf("proof window %d, cache %+v after one 429: want the window kept and nothing cached", cp.ProofWindow, cp.AttestCache)
	}
	var m map[string]any
	b, _ := os.ReadFile(path)
	_ = json.Unmarshal(b, &m)
	if m["want_proof"] != true {
		t.Fatalf("needs file %v: the block 200 below the tip waits for a proof, it is not linked down", m)
	}
	if calls := submittedCalls(t, l2); len(calls) != 0 {
		t.Fatalf("calls %+v after a 429", calls)
	}
}

// I-18, testnet 11-04 09:09: a needed header 14 below the proven tip was
// linked down although the bot's RC paid for none of the 3 runs it needed (one
// run per tick as RC refilled), and no proof was asked for, though a proof's
// EIP-2935 root covers it. Now it asks for a proof; with the RC for it, it
// links down as before.
func TestI18_UnaffordableNearLinkDownAsksForAProof(t *testing.T) {
	const x, tip = uint64(1006), uint64(1020)
	for _, c := range []struct {
		rc        int64
		wantProof bool
	}{{6_325, true}, {1_000_000, false}} {
		rpc, _ := rp9Chain(t, x, tip)
		l2, sub := poolWorld(t)
		l2.set("vsc1bridge", "zkverifier", "vsc1verifier")
		l2.set("vsc1verifier", "h", "1020")
		l2.onDemand = true
		l2.rc = map[string]int64{sub.did.String(): c.rc}
		path := t.TempDir() + "/needs.json"
		t.Setenv("NEEDS_FILE", path)
		cp := &Checkpoint{HeaderNeeds: map[uint64]int64{x: time.Now().Unix()}, ProofWindow: 1}
		handleHeaderNeeds(context.Background(), rpc, l2.gql(t), sub, cp, sub.cfg)
		var m map[string]any
		b, _ := os.ReadFile(path)
		_ = json.Unmarshal(b, &m)
		links := 0
		for _, call := range submittedCalls(t, l2) {
			if call.Action == "submitHeaders" {
				links++
			}
		}
		if (m["want_proof"] == true) != c.wantProof || (links > 0) == c.wantProof {
			t.Fatalf("RC %d: want_proof %v, %d submitHeaders; want a proof asked for = %v (and no link-down then)", c.rc, m["want_proof"], links, c.wantProof)
		}
	}
}

// PS-5 (BTC mapping-bot 4a158b42 parity): the node answers at most 100 state
// keys a query; a read of more failed whole, so with more than 100 header
// needs (an outage's backlog) headersStored failed every tick and no header
// was attested. Reads are paged now.
func TestPS5_MoreThan100StateKeysArePaged(t *testing.T) {
	l2, _ := poolWorld(t)
	l2.set("vsc1bridge", "zkverifier", "vsc1verifier")
	l2.onDemand = true
	var blocks []uint64
	for b := uint64(1000); b < 1150; b++ {
		blocks = append(blocks, b)
	}
	for _, b := range []uint64{1003, 1120, 1149} {
		l2.set("vsc1verifier", "b-"+strconv.FormatUint(b, 10), strings.Repeat("h", 137))
	}
	st, err := headersStored(context.Background(), l2.gql(t), "vsc1bridge", blocks)
	if err != nil {
		t.Fatalf("150 header keys: %v, want them read in pages", err)
	}
	n := 0
	for _, b := range blocks {
		if st[b] {
			n++
		}
	}
	if len(st) != 150 || n != 3 || !st[1120] || !st[1149] {
		t.Fatalf("read %d keys, %d stored (want 150 read, 3 stored incl. 1120 and 1149 past the first page)", len(st), n)
	}
}
