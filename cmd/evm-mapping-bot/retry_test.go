package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const dlTx = "0x00000000000000000000000000000000000000000000000000000000000000aa"

func postRetry(t *testing.T, r *deadLetterRetries, auth, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/retry", strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	w := httptest.NewRecorder()
	r.handler()(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// P7: the retry endpoint is off without OPS_API_KEY, needs the key, validates
// its input, and is throttled per request and per deposit like BTC's /retry.
func TestP7_RetryEndpointAuthValidationAndThrottle(t *testing.T) {
	if code, _ := postRetry(t, newDeadLetterRetries(""), "Bearer x", `{"deposits":["`+dlTx+`"]}`); code != http.StatusForbidden {
		t.Fatalf("no OPS_API_KEY: %d, want 403", code)
	}
	r := newDeadLetterRetries("s3cret")
	cur := time.Unix(1_000_000, 0)
	r.now = func() time.Time { return cur }
	for _, auth := range []string{"", "Bearer wrong", "s3cret", "Basic s3cret"} {
		if code, _ := postRetry(t, r, auth, `{"deposits":["`+dlTx+`"]}`); code != http.StatusUnauthorized {
			t.Fatalf("auth %q: %d, want 401", auth, code)
		}
	}
	for _, body := range []string{`{}`, `{"deposits":[]}`, `{"deposits":["0x12"]}`, `{"deposits":["`+dlTx+`"],"x":1}`, `not json`} {
		if code, _ := postRetry(t, r, "Bearer s3cret", body); code != http.StatusBadRequest {
			t.Fatalf("body %s: %d, want 400", body, code)
		}
	}
	if code, out := postRetry(t, r, "Bearer s3cret", `{"deposits":["`+dlTx+`"]}`); code != http.StatusAccepted || len(out["queued"].([]any)) != 1 {
		t.Fatalf("good request: %d %v, want 202 with one queued", code, out)
	}
	if code, _ := postRetry(t, r, "Bearer s3cret", `{"deposits":["`+dlTx+`"]}`); code != http.StatusTooManyRequests {
		t.Fatalf("inside the global cooldown: %d, want 429", code)
	}
	cur = cur.Add(retryGlobalThrottle + time.Second)
	if code, out := postRetry(t, r, "Bearer s3cret", `{"deposits":["`+dlTx+`"]}`); code != http.StatusAccepted || out["queued"] != nil || len(out["throttled"].([]any)) != 1 {
		t.Fatalf("inside the per-deposit cooldown: %d %v, want 202 with it throttled", code, out)
	}
	if got := r.drain(); len(got) != 1 || got[0] != dlTx {
		t.Fatalf("queued %v, want [%s]", got, dlTx)
	}
}

// P7: a queued retry re-scans the dead letter's block and parks the deposit in
// WaitingDeposits for the normal mapping path; the dead letter and the block's
// "already submitted" mark for it go away. A deposit that is not dead-lettered
// is ignored; a block that cannot be re-scanned keeps the dead letter and the
// request for the next tick.
func TestP7_RequeueMovesTheDeadLetterBackToWaiting(t *testing.T) {
	vault := "0x00000000000000000000000000000000000000fa"
	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Method string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		if fail {
			w.WriteHeader(502)
			return
		}
		switch req.Method {
		case "eth_getBlockByNumber":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"0x65","transactions":[{"hash":"` + dlTx + `","from":"0x00000000000000000000000000000000000000b0","to":"` + vault + `","value":"0x5","transactionIndex":"0x0"}]}}`))
		case "eth_getTransactionReceipt":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"status":"0x1"}}`))
		case "eth_getLogs":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":[]}`))
		default:
			w.WriteHeader(502)
		}
	}))
	t.Cleanup(srv.Close)
	old := callRetryPauses
	callRetryPauses = nil // a failing block fails at once
	t.Cleanup(func() { callRetryPauses = old })
	rpc := newEthRPC(srv.URL)
	cfg := EVMBotConfig{VaultAddress: vault, Tokens: map[string]string{}}
	cp := &Checkpoint{
		DeadLetters: []DeadLetter{{Block: 101, DepositTx: dlTx, Reason: "map failed on chain"}},
		BlockMapped: map[uint64][]string{101: {dlTx + "#-1", "0xother#-1"}},
	}
	r := newDeadLetterRetries("k")
	// a scan failure keeps everything and the request
	fail = true
	r.queued[dlTx] = true
	requeueDeadLetters(rpc, cp, cfg, r)
	if len(cp.DeadLetters) != 1 || len(cp.WaitingDeposits[101]) != 0 || !r.queued[dlTx] {
		t.Fatalf("after a failed re-scan: dead %d waiting %d queued %v; want 1, 0, still queued", len(cp.DeadLetters), len(cp.WaitingDeposits[101]), r.queued)
	}
	fail = false
	r.queued["0x00000000000000000000000000000000000000000000000000000000000000bb"] = true // not dead-lettered
	requeueDeadLetters(rpc, cp, cfg, r)
	if len(cp.DeadLetters) != 0 {
		t.Fatalf("dead letters %v, want none", cp.DeadLetters)
	}
	if w := cp.WaitingDeposits[101]; len(w) != 1 || !strings.EqualFold(w[0].TxHash, dlTx) {
		t.Fatalf("waiting %v, want the deposit at block 101", w)
	}
	if m := cp.BlockMapped[101]; len(m) != 1 || m[0] != "0xother#-1" {
		t.Fatalf("block marks %v, want only the other deposit's", m)
	}
	// a second retry does not park it twice
	cp.DeadLetters = []DeadLetter{{Block: 101, DepositTx: dlTx}}
	r.queued[dlTx] = true
	requeueDeadLetters(rpc, cp, cfg, r)
	if n := len(cp.WaitingDeposits[101]); n != 1 {
		t.Fatalf("waiting %d, want 1 (no duplicate)", n)
	}
}
