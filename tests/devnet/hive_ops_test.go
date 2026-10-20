package devnet

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// A drone that drops the first connection without answering (the stale
// keep-alive failure seen in FDV3 F10 and in P11) must not lose the broadcast:
// BroadcastCustomJSON retries that one error, and the operation reaches Hive
// exactly once.
func TestBroadcastCustomJSONRetriesAStaleKeepAlive(t *testing.T) {
	var requests, broadcasts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if requests.Add(1) == 1 {
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					conn.Close() // closed before the first response byte
					return
				}
			}
		}
		var req struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "condenser_api.get_dynamic_global_properties":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"head_block_number":100,"head_block_id":"0000006400000000000000000000000000000000","time":"2026-10-19T00:00:00"}}`))
		case "condenser_api.broadcast_transaction":
			broadcasts.Add(1)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
		default:
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"unexpected method"}}`))
		}
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())

	cfg := DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.DronePort = port
	d, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.BroadcastCustomJSON("vsc.test", []string{"magi.test1"}, map[string]string{"k": "v"}, cfg.InitminerWIF); err != nil {
		t.Fatalf("broadcast after one dropped connection: %v", err)
	}
	if n := broadcasts.Load(); n != 1 {
		t.Fatalf("operation reached the chain %d times, want exactly 1", n)
	}
	if n := requests.Load(); n < 3 {
		t.Fatalf("only %d requests: the dropped connection was not exercised", n)
	}
}

// Any other broadcast failure is returned, never retried: it may have reached Hive.
func TestBroadcastCustomJSONDoesNotRetryOtherErrors(t *testing.T) {
	var broadcasts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(body), "broadcast_transaction") {
			broadcasts.Add(1)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"missing required active authority"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"head_block_number":100,"head_block_id":"0000006400000000000000000000000000000000","time":"2026-10-19T00:00:00"}}`))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	cfg := DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.DronePort = port
	d, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.BroadcastCustomJSON("vsc.test", []string{"magi.test1"}, map[string]string{"k": "v"}, cfg.InitminerWIF); err == nil {
		t.Fatal("a rejected broadcast returned no error")
	}
	if n := broadcasts.Load(); n != 1 {
		t.Fatalf("rejected broadcast sent %d times, want 1 (no retry)", n)
	}
}

// Every harness broadcast without a retry of its own survives one stale
// keep-alive connection and reaches the chain exactly once (H-7: the ledger
// deposit failed this way in FDV10 TestConsensusDelegatedStakeDevnet).
func TestHarnessBroadcastsRetryAStaleKeepAlive(t *testing.T) {
	cases := map[string]func(d *Devnet) error{
		"BroadcastCustomJSON": func(d *Devnet) error {
			_, err := d.BroadcastCustomJSON("vsc.test", []string{"magi.test1"}, map[string]string{"k": "v"}, d.cfg.InitminerWIF)
			return err
		},
		"Deposit": func(d *Devnet) error {
			_, err := d.Deposit(context.Background(), 1, "1.000", "hive")
			return err
		},
		"fundAccounts": func(d *Devnet) error { return d.fundAccounts() },
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			var requests, broadcasts atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if requests.Add(1) == 1 {
					if hj, ok := w.(http.Hijacker); ok {
						if conn, _, err := hj.Hijack(); err == nil {
							conn.Close()
							return
						}
					}
				}
				w.Header().Set("Content-Type", "application/json")
				if strings.Contains(string(body), "broadcast_transaction") {
					broadcasts.Add(1)
					_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
					return
				}
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"head_block_number":100,"head_block_id":"0000006400000000000000000000000000000000","time":"2026-10-19T00:00:00"}}`))
			}))
			defer srv.Close()
			u, _ := url.Parse(srv.URL)
			port, _ := strconv.Atoi(u.Port())
			cfg := DefaultConfig()
			cfg.DataDir = t.TempDir()
			cfg.DronePort = port
			d, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := call(d); err != nil {
				t.Fatalf("broadcast after one dropped connection: %v", err)
			}
			if n := broadcasts.Load(); n != 1 {
				t.Fatalf("operation reached the chain %d times, want exactly 1", n)
			}
		})
	}
}

// fakeDroneFor serves the two calls a broadcast makes. dropFirst closes the
// first connection unanswered (a stale keep-alive); reject answers every
// broadcast with an error, as a server that read it would.
func fakeDroneFor(t *testing.T, dropFirst, reject bool) (*Devnet, *atomic.Int64) {
	t.Helper()
	var requests, broadcasts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if requests.Add(1) == 1 && dropFirst {
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					conn.Close()
					return
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(body), "broadcast_transaction") {
			broadcasts.Add(1)
			if reject {
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"Assert Exception: Operation JSON must be less than 8192 bytes"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
			return
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"head_block_number":100,"head_block_id":"0000006400000000000000000000000000000000","time":"2026-10-19T00:00:00"}}`))
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	cfg := DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.DronePort = port
	d, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return d, &broadcasts
}

// A contract call is resent only when the server never read it (H-15): a
// resend of one it read could apply the call twice.
func TestCallContractResendsOnlyWhatTheServerNeverRead(t *testing.T) {
	t.Run("stale keep-alive is resent", func(t *testing.T) {
		d, broadcasts := fakeDroneFor(t, true, false)
		if _, err := d.CallContract(context.Background(), 1, "vsc1test", "act", "{}"); err != nil {
			t.Fatalf("call after one dropped connection: %v", err)
		}
		if n := broadcasts.Load(); n != 1 {
			t.Fatalf("call reached the chain %d times, want 1", n)
		}
	})
	t.Run("a call the server read and rejected is not resent", func(t *testing.T) {
		d, broadcasts := fakeDroneFor(t, false, true)
		if _, err := d.CallContract(context.Background(), 1, "vsc1test", "act", "{}"); err == nil {
			t.Fatal("rejected call returned no error")
		}
		if n := broadcasts.Load(); n != 1 {
			t.Fatalf("rejected call sent %d times, want 1", n)
		}
	})
}

func TestBroadcastNeverReachedServer(t *testing.T) {
	cases := map[string]bool{
		"Post \"http://localhost:1\": the server closed connection before returning the first response byte. Make sure the server returns 'Connection: close'": true,
		"Post \"http://localhost:1\": dial tcp 127.0.0.1:1: connect: connection refused":                                                                       true,
		"missing required active authority": false,
		"Post \"http://localhost:1\": context deadline exceeded (Client.Timeout exceeded while awaiting headers)": false,
	}
	for msg, want := range cases {
		if got := broadcastNeverReachedServer(errors.New(msg)); got != want {
			t.Errorf("%q: got %v, want %v", msg, got, want)
		}
	}
	if broadcastNeverReachedServer(nil) {
		t.Error("nil error reported as a failure")
	}
}
