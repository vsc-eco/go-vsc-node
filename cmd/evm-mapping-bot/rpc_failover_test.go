package main

import (
	"context"
	"encoding/json"
	"fmt"
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

// S-1: a transport error carried the endpoint's full URL, API key included,
// into the logs and the /health report. Only the host is kept.
func TestS1_RPCErrorsDoNotCarryTheAPIKey(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close() // connection refused
	_, err := newEthRPC(closed.URL + "/v2/SECRETKEY?key=KEY2").getFinalizedBlock()
	if err == nil {
		t.Fatal("no error from a closed endpoint")
	}
	if strings.Contains(err.Error(), "SECRETKEY") || strings.Contains(err.Error(), "KEY2") || !strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("error %q: want the host only", err)
	}
}

// A19 (BI1): an empty eth_getLogs answer is taken only from an endpoint whose
// head has reached the block. The probe: the primary drops one eth_getLogs
// (HTTP 502), the call fails over to a secondary below the block, which answers
// [], and the deposit's block was marked scanned with nothing found.
func TestA19_EmptyLogsOnlyFromAnEndpointAtTheBlock(t *testing.T) {
	endpoint := func(logs string, logsStatus int, head *atomic.Uint64) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req struct{ Method string }
			_ = json.NewDecoder(r.Body).Decode(&req)
			switch req.Method {
			case "eth_getLogs":
				w.WriteHeader(logsStatus)
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":` + logs + `}`))
			case "eth_blockNumber":
				_, _ = w.Write([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":"0x%x"}`, head.Load())))
			default:
				t.Errorf("unexpected %s", req.Method)
			}
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	var primaryHead, lagHead atomic.Uint64
	primaryHead.Store(200)
	lagHead.Store(80)
	const params = `{"fromBlock":"0x65","toBlock":"0x65"}`
	dropping := endpoint(`[]`, 502, &primaryHead)
	lagging := endpoint(`[]`, 200, &lagHead)
	rpc := newEthRPC(dropping + "," + lagging)
	if _, err := rpc.logsAt(context.Background(), params, 101); err == nil || !strings.Contains(err.Error(), "has not reached block 101") {
		t.Fatalf("empty logs from an endpoint at 80 for block 101: %v, want refused", err)
	}
	lagHead.Store(150) // it catches up
	if res, err := rpc.logsAt(context.Background(), params, 101); err != nil || string(res) != "[]" {
		t.Fatalf("empty logs from an endpoint at 150: %s %v", res, err)
	}
	// A non-empty answer stands whatever the head (no logs past it exist).
	lagHead.Store(80)
	withLog := endpoint(`[{"logIndex":"0x0"}]`, 200, &lagHead)
	if res, err := newEthRPC(dropping+","+withLog).logsAt(context.Background(), params, 101); err != nil || !strings.Contains(string(res), "logIndex") {
		t.Fatalf("a log from a lagging endpoint: %s %v", res, err)
	}
	// A single endpoint is not checked: the block itself came from it.
	if _, err := newEthRPC(endpoint(`[]`, 200, &lagHead)).logsAt(context.Background(), params, 101); err != nil {
		t.Fatalf("single endpoint: %v", err)
	}
}

// A19 end to end: the block comes from the primary, which then drops the
// eth_getLogs call; the secondary is below the block and answers []. The
// scan must fail (the block is retried), not report no deposits, for a token's
// logs and for MagiDeposit's.
func TestA19_ScanFailsRatherThanMissDeposits(t *testing.T) {
	var logsCalls atomic.Int32 // the primary drops its first eth_getLogs only
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Method string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "eth_getBlockByNumber":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"0x65","transactions":[]}}`))
		case "eth_blockNumber":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0xc8"}`))
		case "eth_getLogs":
			if logsCalls.Add(1) == 1 {
				w.WriteHeader(502)
				return
			}
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":[]}`))
		default:
			w.WriteHeader(502)
		}
	}))
	t.Cleanup(primary.Close)
	lagging := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Method string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "eth_blockNumber":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x50"}`))
		case "eth_getLogs":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":[]}`))
		default:
			w.WriteHeader(502)
		}
	}))
	t.Cleanup(lagging.Close)
	vault := "0x00000000000000000000000000000000000000fa"
	for name, tokens := range map[string]map[string]string{
		"token logs":       {"0x1c7d4b196cb0c7b01d743fbc6116a902379c7238": "USDC"},
		"MagiDeposit logs": {},
	} {
		logsCalls.Store(0)
		res, err := scanBlock(newEthRPC(primary.URL+","+lagging.URL), 101, []string{vault}, tokens)
		if err == nil {
			t.Errorf("%s: scan returned %d deposits and no error from a lagging endpoint's empty logs", name, len(res.Deposits))
		}
	}
}
