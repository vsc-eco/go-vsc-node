package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// staleNode is a GraphQL endpoint whose last processed Hive block can move
// (head < 0: the head query fails). It answers getStateByKeys with its name, so
// a test can see which endpoint the walk read.
type staleNode struct {
	name string
	head atomic.Int64
	srv  *httptest.Server
}

func newStaleNode(t *testing.T, name string, head int64) *staleNode {
	n := &staleNode{name: name}
	n.head.Store(head)
	n.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch {
		case strings.Contains(req.Query, "localNodeInfo"):
			h := n.head.Load()
			if h < 0 {
				http.Error(w, "down", http.StatusBadGateway)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"localNodeInfo": map[string]any{"last_processed_block": h}}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"getStateByKeys": map[string]any{"who": n.name}}})
		}
	}))
	t.Cleanup(n.srv.Close)
	return n
}

// I-39, devnet E2r 11-05: the bot's first GraphQL endpoint was cut off from its
// peers and kept answering from a frozen state; the bot read it as truth for 10
// min. Endpoints are ranked by their last processed Hive block: one more than
// gqlStaleBlocks behind the best is read last, and comes back first once it
// catches up. One whose head query fails keeps its place.
func TestI39_AStaleEndpointIsReadLast(t *testing.T) {
	ctx := context.Background()
	a, b, c := newStaleNode(t, "A", 100), newStaleNode(t, "B", 150), newStaleNode(t, "C", 149)
	g := newVSCGraphQL([]string{a.srv.URL, b.srv.URL, c.srv.URL})
	who := func() string {
		st, err := g.fetchContractState(ctx, "vsc1bridge", []string{"who"})
		if err != nil {
			t.Fatal(err)
		}
		return st["who"]
	}
	rank := func() {
		g.rankMu.Lock()
		g.rankedAt = time.Time{}
		g.rankMu.Unlock()
		g.rankByHead(ctx)
	}
	if w := who(); w != "A" {
		t.Fatalf("before any ranking the walk reads %s, want the configured first (A)", w)
	}
	rank()
	if w := who(); w != "B" {
		t.Fatalf("A is 50 blocks behind: the walk reads %s, want B", w)
	}
	if got := g.order(); got[len(got)-1] != a.srv.URL {
		t.Fatalf("the stale endpoint is not last: %v", got)
	}
	// Exactly gqlStaleBlocks behind is still current; one more is not.
	a.head.Store(150 - gqlStaleBlocks)
	rank()
	if w := who(); w != "A" {
		t.Fatalf("A %d behind (the limit): reads %s, want A back first", gqlStaleBlocks, w)
	}
	a.head.Store(150 - gqlStaleBlocks - 1)
	rank()
	if w := who(); w != "B" {
		t.Fatalf("A %d behind: reads %s, want B", gqlStaleBlocks+1, w)
	}
	// Caught up: first again.
	a.head.Store(151)
	rank()
	if w := who(); w != "A" {
		t.Fatalf("A caught up: reads %s, want A", w)
	}
	// A head query that fails keeps the endpoint's place (the walk's failover
	// handles a down endpoint); it is not taken as stale.
	a.head.Store(-1)
	rank()
	if got := g.order(); got[0] != a.srv.URL {
		t.Fatalf("an endpoint whose head query failed moved: %v", got)
	}
	// Ranking runs at most once per gqlRankEvery.
	a.head.Store(1)
	g.rankByHead(ctx)
	if got := g.order(); got[0] != a.srv.URL {
		t.Fatalf("re-ranked inside gqlRankEvery: %v", got)
	}
}
