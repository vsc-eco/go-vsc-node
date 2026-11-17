package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// RETRY-MISLEAD-1 (testnet 11-17): POST /retry answered {"queued":[tx]} for a
// hash that is not dead-lettered, and the tick then ignored it. The reply now
// names it under not_dead_lettered, queues nothing for it, and does not use up
// its cooldown.
func TestRETRYMISLEAD1_TheReplyNamesWhatIsNotDeadLettered(t *testing.T) {
	const other = "0x00000000000000000000000000000000000000000000000000000000000000bb"
	cp := &Checkpoint{DeadLetters: []DeadLetter{{Block: 101, DepositTx: dlTx, Reason: "map failed on chain"}}}
	r := newDeadLetterRetries("k")
	r.isDeadLetter = cp.isDeadLettered
	post := func(body string) (int, map[string][]string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/retry", bytes.NewBufferString(body))
		req.Header.Set("Authorization", "Bearer k")
		w := httptest.NewRecorder()
		r.handler()(w, req)
		var out map[string][]string
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	code, out := post(`{"deposits":["` + other + `","` + dlTx + `"]}`)
	if code != http.StatusAccepted {
		t.Fatalf("status %d, want 202", code)
	}
	if len(out["queued"]) != 1 || out["queued"][0] != dlTx || len(out["not_dead_lettered"]) != 1 || out["not_dead_lettered"][0] != other {
		t.Fatalf("reply %v, want the dead letter queued and the other hash under not_dead_lettered", out)
	}
	if r.queued[strings.ToLower(other)] || !r.queued[strings.ToLower(dlTx)] {
		t.Fatalf("queue %v, want only the dead letter", r.queued)
	}
	if _, used := r.lastPer[strings.ToLower(other)]; used {
		t.Fatal("a hash that is not dead-lettered used up its cooldown")
	}
}
