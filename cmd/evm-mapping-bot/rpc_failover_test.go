package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// B14: ETH_RPC may list several endpoints; a call moves on only when an
// endpoint does not answer, not when the chain answers with an error.
func TestB14_EthRPCFailsOverOnlyWhenAnEndpointDoesNotAnswer(t *testing.T) {
	serve := func(status int, body string) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	down := serve(200, "")
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close() // connection refused
	good := serve(200, `{"jsonrpc":"2.0","id":1,"result":{"number":"0x2a"}}`)
	for name, tc := range map[string]struct {
		urls    []string
		want    uint64
		wantErr string
	}{
		"first refuses connections": {urls: []string{closed.URL, good}, want: 42},
		"first answers HTTP 503":    {urls: []string{serve(503, "busy"), good}, want: 42},
		"first sends garbage":       {urls: []string{serve(200, "<html>"), good}, want: 42},
		"first sends nothing":       {urls: []string{down, good}, want: 42},
		"a JSON-RPC error is final": {urls: []string{serve(200, `{"jsonrpc":"2.0","id":1,"error":{"message":"nope"}}`), good}, wantErr: "rpc error: nope"},
		"a null result is final":    {urls: []string{serve(200, `{"jsonrpc":"2.0","id":1,"result":null}`), good}, wantErr: "null result"},
		"none answers":              {urls: []string{closed.URL, serve(502, "")}, wantErr: "rpc HTTP 502"},
	} {
		rpc := newEthRPC(strings.Join(tc.urls, " , "))
		got, err := rpc.getFinalizedBlock()
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%s: block %d err %v, want %q", name, got, err, tc.wantErr)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%s: block %d err %v, want %d", name, got, err, tc.want)
		}
	}
	if _, err := newEthRPC("").getFinalizedBlock(); err == nil {
		t.Error("no endpoint configured: no error")
	}
}

// The startup log names RPC endpoints by host: their URLs carry API keys.
func TestB14_RPCHostsHidesKeys(t *testing.T) {
	got := rpcHosts("https://eth-sepolia.g.alchemy.com/v2/SECRETKEY, https://sepolia.infura.io/v3/KEY2?x=1,not a url")
	if strings.Contains(got, "SECRET") || strings.Contains(got, "KEY2") || got != "eth-sepolia.g.alchemy.com,sepolia.infura.io,(unparsed)" {
		t.Fatalf("rpcHosts = %q", got)
	}
}

// B-R4: every call started at the first endpoint, so a primary that does not
// answer cost each call its timeout. Calls now start at the endpoint that last
// answered.
func TestBR4_FailoverSticksToTheEndpointThatAnswered(t *testing.T) {
	var primaryHits, backupHits atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryHits.Add(1)
		w.WriteHeader(503)
	}))
	t.Cleanup(primary.Close)
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupHits.Add(1)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"0x2a"}}`))
	}))
	t.Cleanup(backup.Close)
	rpc := newEthRPC(primary.URL + "," + backup.URL)
	for i := 0; i < 3; i++ {
		if got, err := rpc.getFinalizedBlock(); err != nil || got != 42 {
			t.Fatalf("call %d: %d %v", i, got, err)
		}
	}
	if p, b := primaryHits.Load(), backupHits.Load(); p != 1 || b != 3 {
		t.Fatalf("primary asked %d times, backup %d; want the primary once", p, b)
	}
}
