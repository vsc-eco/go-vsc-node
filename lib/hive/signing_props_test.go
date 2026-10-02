package hive_test

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"vsc-node/lib/hive"

	"github.com/vsc-eco/hivego"
)

// dgpServer answers get_dynamic_global_properties, but closes the connection
// without a response for the first `drop` requests, the way an API server does
// to a keep-alive connection it has just closed as idle.
func dgpServer(t *testing.T, drop int32) (*httptest.Server, *atomic.Int32) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= drop {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			conn.Close()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"head_block_number":123,` +
			`"head_block_id":"0000007bdeadbeefdeadbeefdeadbeefdeadbeef","time":"2026-10-02T00:00:00"}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// One dropped connection must not leave the transaction without signing props
// (an empty expiration makes Sign fail, which dropped TSS commitments).
func TestPopulateSigningProps_RetriesDroppedConnection(t *testing.T) {
	srv, calls := dgpServer(t, 1)
	b := hive.TransactionBroadcaster{Client: hivego.NewHiveRpc([]string{srv.URL})}

	tx := hivego.HiveTransaction{}
	if err := b.PopulateSigningProps(&tx, nil); err != nil {
		t.Fatalf("PopulateSigningProps after one dropped connection: %v", err)
	}
	if tx.Expiration != "2026-10-02T00:00:30" || tx.RefBlockNum != 123 {
		t.Fatalf("got expiration %q ref %d, want 2026-10-02T00:00:30 and 123", tx.Expiration, tx.RefBlockNum)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("%d requests, want 2 (one dropped, one answered)", got)
	}
}

// A server that keeps failing still yields an error, after a bounded number of tries.
func TestPopulateSigningProps_GivesUp(t *testing.T) {
	srv, calls := dgpServer(t, 1000)
	b := hive.TransactionBroadcaster{Client: hivego.NewHiveRpc([]string{srv.URL})}

	tx := hivego.HiveTransaction{}
	if err := b.PopulateSigningProps(&tx, nil); err == nil {
		t.Fatal("want an error when every request is dropped")
	}
	if tx.Expiration != "" {
		t.Fatalf("expiration set to %q on failure", tx.Expiration)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("%d requests, want 3", got)
	}
}
