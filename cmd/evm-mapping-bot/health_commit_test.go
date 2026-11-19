package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"vsc-node/lib/buildinfo"
)

// /health names the commit the bot was built from, in every state (the 11-08
// mainnet BTC bot ran an old branch and nothing on /health showed it).
func TestHealthReportsGitCommit(t *testing.T) {
	old := buildinfo.GitCommit
	defer func() { buildinfo.GitCommit = old }()
	buildinfo.GitCommit = "feedc0de"

	h := newBotHealth(12 * time.Second)
	if r := h.report(time.Now()); r.Status != "starting" || r.GitCommit != "feedc0de" {
		t.Fatalf("starting report: %+v", r)
	}
	h.recordTick(time.Now(), nil, emptyCP())
	if r := h.report(time.Now()); r.Status != "ok" || r.GitCommit != "feedc0de" {
		t.Fatalf("ok report: %+v", r)
	}

	srv := httptest.NewServer(h.handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	if raw["gitCommit"] != "feedc0de" {
		t.Fatalf("GET /health gitCommit = %v, want feedc0de (body %v)", raw["gitCommit"], raw)
	}
}
