package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// twoNodeWorld is stallWorld with a second endpoint ("node B") in front of the
// same chain. A transaction only node A received is stuck (l2.pool.stuck);
// node B receiving the same bytes is it reaching the nodes a block needs, so
// the stuck mark goes.
func twoNodeWorld(t *testing.T) (*fakeL2, *l2Submitter, *time.Time, *atomic.Int32) {
	t.Helper()
	l2, sub, cur := stallWorld(t)
	a := sub.gql.urls[0]
	var bSubmits atomic.Int32
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.Unmarshal(body, &req)
		if strings.Contains(req.Query, "submitTransactionV1") {
			bSubmits.Add(1)
			raw, _ := base64.URLEncoding.DecodeString(req.Variables["tx"].(string))
			l2.mu.Lock()
			delete(l2.pool.stuck, localTxID(raw))
			l2.mu.Unlock()
		}
		resp, err := http.Post(a, "application/json", bytes.NewReader(body))
		if err != nil {
			w.WriteHeader(502)
			return
		}
		defer resp.Body.Close()
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(b.Close)
	sub.gql.urls = []string{a, b.URL}
	return l2, sub, cur, &bSubmits
}

// I-8, testnet 11-04 03:54: the bot's node held its transactions and never
// gossiped them; the account nonce stopped and, with the bot's work waiting on
// them, no new call came, so B-R1b (which resends on a new call) never fired.
// The timer re-broadcasts the same bytes to every endpoint once the account
// nonce has not moved for stallRebroadcastAfter, and they land, with no new
// call and no nonce reused.
func TestI8_StuckTransactionsAreRebroadcastWithoutANewCall(t *testing.T) {
	l2, sub, cur, bSubmits := twoNodeWorld(t)
	a, err := mapCall(t, sub, 1) // nonce 0 to node A only
	if err != nil {
		t.Fatal(err)
	}
	b, err := mapCall(t, sub, 2) // nonce 1, also node A only
	if err != nil {
		t.Fatal(err)
	}
	l2.mu.Lock()
	l2.pool.stuck = map[string]bool{a: true, b: true}
	l2.mu.Unlock()
	l2.pool.produce()
	sub.rebroadcastStalled(context0()) // starts the clock
	*cur = cur.Add(stallRebroadcastAfter - time.Second)
	sub.rebroadcastStalled(context0())
	if n := bSubmits.Load(); n != 0 {
		t.Fatalf("%d re-broadcasts inside the window", n)
	}
	*cur = cur.Add(2 * time.Second)
	sub.rebroadcastStalled(context0())
	if n := bSubmits.Load(); n != 2 {
		t.Fatalf("%d re-broadcasts to node B, want both stuck transactions", n)
	}
	l2.pool.produce()
	if l2.pool.confirmed != 2 {
		t.Fatalf("account nonce %d after the re-broadcast, want 2 (both landed, no new call)", l2.pool.confirmed)
	}
	if got := submittedNonces(t, l2); !reflect.DeepEqual(got, []uint64{0, 1, 0, 0, 1, 1}) {
		t.Fatalf("nonces %v, want [0 1] sent, then each again to A and B", got)
	}
	l2.mu.Lock()
	same := bytes.Equal(l2.submitted[2], l2.submitted[0]) && bytes.Equal(l2.submitted[3], l2.submitted[0]) &&
		bytes.Equal(l2.submitted[4], l2.submitted[1]) && bytes.Equal(l2.submitted[5], l2.submitted[1])
	l2.mu.Unlock()
	if !same {
		t.Fatal("a re-broadcast is not the same bytes: it signed something new")
	}
	// landed: nothing held, nothing more re-broadcast
	*cur = cur.Add(time.Hour)
	sub.rebroadcastStalled(context0())
	if n := bSubmits.Load(); n != 2 {
		t.Fatalf("%d re-broadcasts after they landed", n)
	}
}

// A moving account nonce is no stall, and an idle stretch (nothing held) is
// not either: the clock starts when the bot has something above it (I-19).
func TestI8_NoRebroadcastWhileTheChainMovesOrTheBotIsIdle(t *testing.T) {
	l2, sub, cur, bSubmits := twoNodeWorld(t)
	sub.rebroadcastStalled(context0())
	*cur = cur.Add(time.Hour) // idle: nothing held
	sub.rebroadcastStalled(context0())
	if _, err := mapCall(t, sub, 1); err != nil {
		t.Fatal(err)
	}
	sub.rebroadcastStalled(context0()) // first sight of a held call: the clock starts now, not an hour ago
	if n := bSubmits.Load(); n != 0 {
		t.Fatalf("%d re-broadcasts right after an idle hour", n)
	}
	for i := 0; i < 5; i++ { // the chain takes each call within the window
		l2.pool.produce()
		if _, err := mapCall(t, sub, i+2); err != nil {
			t.Fatal(err)
		}
		*cur = cur.Add(stallRebroadcastAfter - time.Second)
		sub.rebroadcastStalled(context0())
	}
	if n := bSubmits.Load(); n != 0 {
		t.Fatalf("%d re-broadcasts while the account nonce kept moving", n)
	}
}

func context0() context.Context { return context.Background() }
