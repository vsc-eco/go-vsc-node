package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ----- DashdRPCClient against a mock JSON-RPC server -----

func mockDashdServer(t *testing.T, handlers map[string]func(params []any) (any, error)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req rpcReq
		require.NoError(t, json.Unmarshal(body, &req))
		h, ok := handlers[req.Method]
		if !ok {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(rpcResp{Error: &rpcError{Code: -32601, Message: "method not found: " + req.Method}})
			return
		}
		result, err := h(req.Params)
		if err != nil {
			_ = json.NewEncoder(w).Encode(rpcResp{Error: &rpcError{Code: -32000, Message: err.Error()}})
			return
		}
		raw, _ := json.Marshal(result)
		_ = json.NewEncoder(w).Encode(rpcResp{Result: raw})
	}))
	return srv
}

func TestDashdRPC_GetRawMempool(t *testing.T) {
	srv := mockDashdServer(t, map[string]func([]any) (any, error){
		"getrawmempool": func([]any) (any, error) {
			return []string{"abc123", "def456"}, nil
		},
	})
	defer srv.Close()

	c := NewDashdRPCClient(srv.URL, "user", "pass")
	ids, err := c.GetRawMempool(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"abc123", "def456"}, ids)
}

func TestDashdRPC_GetRawTransaction_Locked(t *testing.T) {
	srv := mockDashdServer(t, map[string]func([]any) (any, error){
		"getrawtransaction": func(params []any) (any, error) {
			return map[string]any{
				"txid":        "abc123",
				"hex":         "deadbeef",
				"instantlock": true,
			}, nil
		},
	})
	defer srv.Close()

	c := NewDashdRPCClient(srv.URL, "user", "pass")
	tx, err := c.GetRawTransaction(context.Background(), "abc123")
	require.NoError(t, err)
	assert.Equal(t, "abc123", tx.TxID)
	assert.Equal(t, "deadbeef", tx.Hex)
	assert.True(t, tx.InstantLock)
}

func TestDashdRPC_GetRawTransaction_NotLocked(t *testing.T) {
	srv := mockDashdServer(t, map[string]func([]any) (any, error){
		"getrawtransaction": func(params []any) (any, error) {
			return map[string]any{
				"txid":        "abc123",
				"hex":         "deadbeef",
				"instantlock": false,
			}, nil
		},
	})
	defer srv.Close()

	c := NewDashdRPCClient(srv.URL, "user", "pass")
	tx, err := c.GetRawTransaction(context.Background(), "abc123")
	require.NoError(t, err)
	assert.False(t, tx.InstantLock)
}

func TestDashdRPC_ErrorPropagation(t *testing.T) {
	srv := mockDashdServer(t, map[string]func([]any) (any, error){
		"getrawmempool": func([]any) (any, error) {
			return nil, assertSentinelErr{}
		},
	})
	defer srv.Close()

	c := NewDashdRPCClient(srv.URL, "user", "pass")
	_, err := c.GetRawMempool(context.Background())
	assert.Error(t, err)
}

type assertSentinelErr struct{}

func (assertSentinelErr) Error() string { return "synthetic test error" }

// ----- DashdWatcher poll loop -----

func TestWatcher_FiresOnInstantLock(t *testing.T) {
	// Mock dashd: getrawmempool returns one txid; getrawtransaction
	// returns it with instantlock=true.
	var rpcCalls atomic.Int32
	srv := mockDashdServer(t, map[string]func([]any) (any, error){
		"getrawmempool": func([]any) (any, error) {
			rpcCalls.Add(1)
			return []string{"abc123"}, nil
		},
		"getrawtransaction": func(params []any) (any, error) {
			return map[string]any{
				"txid":        "abc123",
				"hex":         "deadbeef",
				"instantlock": true,
				"vout": []map[string]any{
					{"scriptPubKey": map[string]any{
						"addresses": []string{"8WatchedAddrTestStub"},
					}},
				},
			}, nil
		},
	})
	defer srv.Close()

	client := NewDashdRPCClient(srv.URL, "user", "pass")
	w := NewDashdWatcher(client)
	w.pollInterval = 50 * time.Millisecond // tight loop for tests

	var observed struct {
		mu       sync.Mutex
		sid      string
		txid     string
		rawTxHex string
		fired    int
	}
	w.Watch("8WatchedAddrTestStub", "sid-1", func(sid, txid, rawTxHex string) {
		observed.mu.Lock()
		defer observed.mu.Unlock()
		observed.sid = sid
		observed.txid = txid
		observed.rawTxHex = rawTxHex
		observed.fired++
	})

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_ = w.Run(ctx) // will return ctx.Err() at deadline

	observed.mu.Lock()
	defer observed.mu.Unlock()
	assert.Equal(t, "sid-1", observed.sid)
	assert.Equal(t, "abc123", observed.txid)
	assert.Equal(t, "deadbeef", observed.rawTxHex)
	assert.GreaterOrEqual(t, observed.fired, 1, "callback must fire at least once")
	assert.LessOrEqual(t, observed.fired, 1, "with seen-map, callback must NOT fire repeatedly for the same txid")
}

func TestWatcher_SkipsUnlocked(t *testing.T) {
	srv := mockDashdServer(t, map[string]func([]any) (any, error){
		"getrawmempool": func([]any) (any, error) {
			return []string{"unlocked-tx"}, nil
		},
		"getrawtransaction": func(params []any) (any, error) {
			return map[string]any{
				"txid":        "unlocked-tx",
				"hex":         "00",
				"instantlock": false,
			}, nil
		},
	})
	defer srv.Close()

	client := NewDashdRPCClient(srv.URL, "user", "pass")
	w := NewDashdWatcher(client)
	w.pollInterval = 50 * time.Millisecond

	fired := atomic.Int32{}
	w.Watch("any-address", "sid-1", func(string, string, string) { fired.Add(1) })

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	_ = w.Run(ctx)

	assert.Equal(t, int32(0), fired.Load(),
		"unlocked tx must not fire observation callback")
}

func TestWatcher_UnwatchStopsCallbacks(t *testing.T) {
	srv := mockDashdServer(t, map[string]func([]any) (any, error){
		"getrawmempool":     func([]any) (any, error) { return []string{}, nil },
		"getrawtransaction": func([]any) (any, error) { return nil, nil },
	})
	defer srv.Close()

	w := NewDashdWatcher(NewDashdRPCClient(srv.URL, "u", "p"))
	w.Watch("addr", "sid", func(string, string, string) {})
	assert.Equal(t, 1, mapLen(w))
	w.Unwatch("addr")
	assert.Equal(t, 0, mapLen(w))
}

func mapLen(w *DashdWatcher) int {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return len(w.watched)
}

// TestWatcher_OnlyFiresForMatchingPayee — regression for the audit's
// `watcher-fires-onobserved-cross-product` finding. Without the per-output
// address match, an IS-locked tx paying address X would fan out to every
// watched session, not just the session expecting X.
func TestWatcher_OnlyFiresForMatchingPayee(t *testing.T) {
	srv := mockDashdServer(t, map[string]func([]any) (any, error){
		"getrawmempool": func([]any) (any, error) {
			return []string{"locked-tx"}, nil
		},
		"getrawtransaction": func(params []any) (any, error) {
			return map[string]any{
				"txid":        "locked-tx",
				"hex":         "deadbeef",
				"instantlock": true,
				"vout": []map[string]any{
					// tx pays addressA only — addressB watcher must NOT fire.
					{"scriptPubKey": map[string]any{
						"addresses": []string{"addressA"},
					}},
				},
			}, nil
		},
	})
	defer srv.Close()

	w := NewDashdWatcher(NewDashdRPCClient(srv.URL, "u", "p"))
	w.pollInterval = 50 * time.Millisecond

	firedA := atomic.Int32{}
	firedB := atomic.Int32{}
	w.Watch("addressA", "sid-a", func(string, string, string) { firedA.Add(1) })
	w.Watch("addressB", "sid-b", func(string, string, string) { firedB.Add(1) })

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_ = w.Run(ctx)

	assert.Equal(t, int32(1), firedA.Load(), "addressA watcher must fire exactly once")
	assert.Equal(t, int32(0), firedB.Load(),
		"addressB watcher must NOT fire (tx didn't pay addressB) — see audit cross-product fan-out")
}

// TestWatcher_SkipsTxWithNoDecodableVout — defensive behavior: if dashd
// returns a tx whose outputs we can't decode (no scriptPubKey.addresses),
// the watcher must NOT fan-out blindly; it should skip.
func TestWatcher_SkipsTxWithNoDecodableVout(t *testing.T) {
	srv := mockDashdServer(t, map[string]func([]any) (any, error){
		"getrawmempool": func([]any) (any, error) {
			return []string{"opaque-tx"}, nil
		},
		"getrawtransaction": func(params []any) (any, error) {
			return map[string]any{
				"txid":        "opaque-tx",
				"hex":         "deadbeef",
				"instantlock": true,
				"vout":        []map[string]any{},
			}, nil
		},
	})
	defer srv.Close()

	w := NewDashdWatcher(NewDashdRPCClient(srv.URL, "u", "p"))
	w.pollInterval = 50 * time.Millisecond

	fired := atomic.Int32{}
	w.Watch("any-address", "sid", func(string, string, string) { fired.Add(1) })

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_ = w.Run(ctx)

	assert.Equal(t, int32(0), fired.Load(),
		"undecodable vout must not trigger fan-out")
}

func TestPayeeAddresses_DedupesAndAcceptsBothFields(t *testing.T) {
	r := &RawTransactionResult{
		Vout: []RawTxOutput{
			{ScriptPubKey: RawTxScriptPubKey{Addresses: []string{"x", "y"}}},
			// Legacy `address` singular field — should be merged.
			{ScriptPubKey: RawTxScriptPubKey{Address: "z"}},
			// Duplicate "x" must dedupe.
			{ScriptPubKey: RawTxScriptPubKey{Addresses: []string{"x"}}},
		},
	}
	got := r.payeeAddresses()
	assert.ElementsMatch(t, []string{"x", "y", "z"}, got)
}

func TestWatcher_NoWatchedAddressesIsNoOp(t *testing.T) {
	srv := mockDashdServer(t, map[string]func([]any) (any, error){
		// Should never be called.
		"getrawmempool": func([]any) (any, error) {
			t.Error("getrawmempool should not be called with no watched addresses")
			return []string{}, nil
		},
	})
	defer srv.Close()

	w := NewDashdWatcher(NewDashdRPCClient(srv.URL, "u", "p"))
	w.pollInterval = 50 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_ = w.Run(ctx)
	// Test passes if t.Error wasn't triggered.
}

// ----- Server.onISLockObserved -----

func TestServer_OnISLockObserved_TransitionsState(t *testing.T) {
	srv := newTestServer(t)

	// Manually populate a session.
	now := time.Now()
	sess := &Session{
		Sid:            "sid-x",
		Op:             OpAuth,
		DepositAddress: "8TestStubP2SH...",
		State:          StateWaitingForIS,
		CreatedAt:      now,
		ExpiresAt:      now.Add(30 * time.Minute),
	}
	_ = srv.sessions.Put(sess)

	srv.onISLockObserved("sid-x", "txidABC", "deadbeef")

	got, ok := srv.sessions.Get("sid-x")
	require.True(t, ok)
	assert.Equal(t, StateISObserved, got.State)
	assert.Equal(t, "txidABC", got.DashTxId)
}

func TestServer_OnISLockObserved_IdempotentIfPastWaiting(t *testing.T) {
	srv := newTestServer(t)

	now := time.Now()
	sess := &Session{
		Sid:       "sid-y",
		Op:        OpAuth,
		State:     StateAttesting, // already past WAITING_FOR_IS
		CreatedAt: now,
		ExpiresAt: now.Add(30 * time.Minute),
	}
	_ = srv.sessions.Put(sess)

	// Second observation shouldn't overwrite anything.
	srv.onISLockObserved("sid-y", "wrongTxid", "wronghex")

	got, ok := srv.sessions.Get("sid-y")
	require.True(t, ok)
	assert.Equal(t, StateAttesting, got.State,
		"onISLockObserved must NOT advance state past WAITING_FOR_IS")
	assert.NotEqual(t, "wrongTxid", got.DashTxId)
}

func TestServer_OnISLockObserved_UnknownSidIsNoOp(t *testing.T) {
	srv := newTestServer(t)
	// Just must not panic.
	srv.onISLockObserved("nonexistent", "txid", "hex")
}
