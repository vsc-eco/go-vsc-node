package gql

import (
	"net/http/httptest"
	"testing"
)

// review2 HIGH #18 — before the fix, GET /health (and any non-GraphQL path)
// fell through the mux and did not return a usable liveness signal; external
// probes could not distinguish "node up" from "serving stale HTML".
//
// Differential: on the #170 baseline there is no /health route, so the real
// production handler returns 404 → this test FAILS (RED). On fix/review2 the
// route returns 200 application/json {"status":"ok"} → PASS (GREEN).
//
// Uses makeTestHandler (hardening_test.go) which wires the actual
// gqlManager.Init pipeline — same handler the production server serves.
func TestReview2HealthEndpoint(t *testing.T) {
	h := makeTestHandler(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/health", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("GET /health = %d, want 200 (review2 #18: liveness endpoint missing)", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("GET /health Content-Type = %q, want application/json", ct)
	}
	if body := rec.Body.String(); body != `{"status":"ok"}` {
		t.Fatalf("GET /health body = %q, want %q", body, `{"status":"ok"}`)
	}
}
