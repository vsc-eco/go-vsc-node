package mapper

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hasura/go-graphql-client"
)

// i39Node is a GraphQL endpoint whose last processed Hive block can move
// (head < 0: the head query fails); any other query answers with its name.
type i39Node struct {
	name string
	head atomic.Int64
	srv  *httptest.Server
}

func newI39Node(t *testing.T, name string, head int64) *i39Node {
	n := &i39Node{name: name}
	n.head.Store(head)
	n.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), "localNodeInfo") {
			h := n.head.Load()
			if h < 0 {
				http.Error(w, "down", http.StatusBadGateway)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"localNodeInfo": map[string]any{"last_processed_block": h}}})
			return
		}
		_, _ = w.Write([]byte(n.name))
	}))
	t.Cleanup(n.srv.Close)
	return n
}

// I-39 (the BTC bot walks its GraphQL endpoints like the EVM bot): an endpoint
// more than gqlStaleBlocks behind the best is walked last by both walks
// (gqlHTTPPost, gqlClientDo), comes back first once current, and one whose head
// query fails keeps its place. A single-endpoint override is never reordered.
func TestI39_BTCBotReadsAStaleEndpointLast(t *testing.T) {
	ctx := context.Background()
	a, b, c := newI39Node(t, "A", 100), newI39Node(t, "B", 150), newI39Node(t, "C", 149)
	urls := []string{a.srv.URL, b.srv.URL, c.srv.URL}
	clients := []*graphql.Client{graphql.NewClient(urls[0], http.DefaultClient), graphql.NewClient(urls[1], http.DefaultClient), graphql.NewClient(urls[2], http.DefaultClient)}
	names := map[*graphql.Client]string{clients[0]: "A", clients[1]: "B", clients[2]: "C"}
	bot := &Bot{gqlURLs: urls, gqlClients: clients, L: slog.Default()}
	rank := func() {
		bot.gqlRankMu.Lock()
		bot.gqlRankedAt = time.Time{}
		bot.gqlRankMu.Unlock()
		bot.RankGqlByHead(ctx)
	}
	viaHTTP := func() string {
		var got string
		if err := bot.gqlHTTPPost(ctx, []byte(`{"query":"x"}`), func(r *http.Response) error {
			b, _ := io.ReadAll(r.Body)
			got = string(b)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return got
	}
	viaClient := func() string {
		var got string
		_ = bot.gqlClientDo(ctx, func(cl *graphql.Client) error { got = names[cl]; return nil })
		return got
	}
	if h, cl := viaHTTP(), viaClient(); h != "A" || cl != "A" {
		t.Fatalf("before ranking: %s / %s, want A (as configured)", h, cl)
	}
	rank()
	if h, cl := viaHTTP(), viaClient(); h != "B" || cl != "B" {
		t.Fatalf("A 50 blocks behind: %s / %s, want B", h, cl)
	}
	a.head.Store(150 - gqlStaleBlocks)
	rank()
	if h := viaHTTP(); h != "A" {
		t.Fatalf("A %d behind (the limit): %s, want A", gqlStaleBlocks, h)
	}
	a.head.Store(150 - gqlStaleBlocks - 1)
	rank()
	if h := viaHTTP(); h != "B" {
		t.Fatalf("A %d behind: %s, want B", gqlStaleBlocks+1, h)
	}
	a.head.Store(-1) // the head query fails: keeps its place
	rank()
	if h := viaHTTP(); h != "A" {
		t.Fatalf("A's head query failed: %s, want A kept first", h)
	}
	// A single-endpoint override is never reordered.
	b.head.Store(1)
	over := &Bot{GqlURL: b.srv.URL, gqlURLs: urls, gqlClients: clients, L: slog.Default()}
	over.RankGqlByHead(ctx)
	if got := over.gqlOrder(1); len(got) != 1 || got[0] != 0 {
		t.Fatalf("override order %v", got)
	}
}
