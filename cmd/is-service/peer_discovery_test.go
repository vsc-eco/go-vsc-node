package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeGQL stands up an httptest.Server that replies with the given JSON
// body to any POST, regardless of query. Used to exercise the discovery
// parser without touching a real magi node.
func fakeGQL(t *testing.T, respBody string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
			http.Error(w, "wrong method", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(respBody))
	}))
}

func TestDiscoverBootstrapPeers_FlatMaps(t *testing.T) {
	// Two enabled witnesses each with multiple peer_addrs → flat-mapped.
	tr := true
	body := mustJSON(t, map[string]any{
		"data": map[string]any{
			"witnessNodes": []map[string]any{
				{"account": "a", "enabled": tr, "peer_addrs": []string{"/ip4/1.1.1.1/tcp/9000/p2p/12D3KooWA", "/ip4/2.2.2.2/tcp/9000/p2p/12D3KooWA"}},
				{"account": "b", "enabled": tr, "peer_addrs": []string{"/ip4/3.3.3.3/tcp/9000/p2p/12D3KooWB"}},
			},
		},
	})
	srv := fakeGQL(t, body)
	defer srv.Close()

	peers, err := discoverBootstrapPeers(context.Background(), srv.URL, 1)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got, want := len(peers), 3; got != want {
		t.Fatalf("expected %d peers (2 from A flat-mapped + 1 from B), got %d: %v", want, got, peers)
	}
	for _, p := range peers {
		if !strings.HasPrefix(p, "/ip4/") {
			t.Errorf("expected /ip4/ prefix on each peer addr, got %q", p)
		}
	}
}

func TestDiscoverBootstrapPeers_SkipsDisabledAndEmpty(t *testing.T) {
	tr := true
	fa := false
	body := mustJSON(t, map[string]any{
		"data": map[string]any{
			"witnessNodes": []map[string]any{
				{"account": "active", "enabled": tr, "peer_addrs": []string{"/ip4/1.1.1.1/tcp/9000/p2p/12D3"}},
				{"account": "disabled", "enabled": fa, "peer_addrs": []string{"/ip4/2.2.2.2/tcp/9000/p2p/12D3"}},
				{"account": "no-addrs", "enabled": tr, "peer_addrs": []string{}},
				{"account": "nil-enabled", "enabled": nil, "peer_addrs": []string{"/ip4/3.3.3.3/tcp/9000/p2p/12D3"}},
			},
		},
	})
	srv := fakeGQL(t, body)
	defer srv.Close()

	peers, err := discoverBootstrapPeers(context.Background(), srv.URL, 1)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got, want := len(peers), 1; got != want {
		t.Fatalf("expected only 'active' to make it through, got %d: %v", got, peers)
	}
	if !strings.Contains(peers[0], "1.1.1.1") {
		t.Errorf("expected 1.1.1.1 (the only enabled+addrs witness), got %q", peers[0])
	}
}

func TestDiscoverBootstrapPeers_ZeroResultsErrorsLoud(t *testing.T) {
	tr := true
	fa := false
	body := mustJSON(t, map[string]any{
		"data": map[string]any{
			"witnessNodes": []map[string]any{
				{"account": "disabled", "enabled": fa, "peer_addrs": []string{"/ip4/2.2.2.2/tcp/9000/p2p/12D3"}},
				{"account": "no-addrs", "enabled": tr, "peer_addrs": []string{}},
			},
		},
	})
	srv := fakeGQL(t, body)
	defer srv.Close()

	_, err := discoverBootstrapPeers(context.Background(), srv.URL, 1)
	if err == nil {
		t.Fatal("expected an error when 0 usable peers — got nil (would have silently started in degraded mode)")
	}
	// The error should surface the breakdown — disabled vs no-addrs vs total —
	// so the operator can diagnose why discovery returned nothing without
	// having to enable extra logging.
	for _, want := range []string{"total=2", "skipped_disabled=1", "skipped_no_addrs=1", "refusing"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should contain %q for diagnosability, got: %v", want, err)
		}
	}
}

func TestDiscoverBootstrapPeers_GraphQLErrorsBubble(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"data": nil,
		"errors": []map[string]any{
			{"message": "field witnessNodes does not exist"},
		},
	})
	srv := fakeGQL(t, body)
	defer srv.Close()

	_, err := discoverBootstrapPeers(context.Background(), srv.URL, 1)
	if err == nil {
		t.Fatal("expected error on GraphQL errors response; got nil")
	}
	if !strings.Contains(err.Error(), "field witnessNodes does not exist") {
		t.Errorf("expected upstream GraphQL error message in returned err, got: %v", err)
	}
}

func TestDiscoverBootstrapPeers_HTTPNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer srv.Close()

	_, err := discoverBootstrapPeers(context.Background(), srv.URL, 1)
	if err == nil {
		t.Fatal("expected error on non-2xx response; got nil")
	}
	if !strings.Contains(err.Error(), "http 502") {
		t.Errorf("expected http status code in error, got: %v", err)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshalling test fixture: %v", err)
	}
	return string(b)
}
