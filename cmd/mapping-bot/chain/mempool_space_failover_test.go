package chain

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// B7 (EVM bot B14 parity): a chain API endpoint that does not answer (HTTP
// 5xx, 429, network error) fails over to the next and the client stays on the
// one that answered; an answer, 404 included, stands.
func TestB7_ChainAPIFailsOverOnlyWhenAnEndpointDoesNotAnswer(t *testing.T) {
	var aCalls, bCalls atomic.Int32
	aStatus := atomic.Int32{}
	aStatus.Store(http.StatusServiceUnavailable)
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		aCalls.Add(1)
		if s := int(aStatus.Load()); s != 200 {
			w.WriteHeader(s)
			return
		}
		_, _ = w.Write([]byte("111"))
	}))
	t.Cleanup(a.Close)
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bCalls.Add(1)
		switch r.URL.Path {
		case "/blocks/tip/height":
			_, _ = w.Write([]byte("222"))
		case "/tx":
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(b.Close)

	c := NewMempoolSpaceClient(&http.Client{Timeout: 5 * time.Second}, "https://unused.example")
	c.SetBaseURLs([]string{" " + a.URL + "/ ", "", b.URL})
	if got := c.BaseURLs(); len(got) != 2 || got[0] != a.URL {
		t.Fatalf("base URLs %v, want the two trimmed endpoints", got)
	}
	h, err := c.GetTipHeight()
	if err != nil || h != 222 {
		t.Fatalf("tip %d err %v, want 222 from the second endpoint after a 503", h, err)
	}
	// sticky: the next call goes to the endpoint that answered
	before := aCalls.Load()
	if _, err := c.GetTipHeight(); err != nil || aCalls.Load() != before {
		t.Fatalf("err %v; the first endpoint was asked again (%d -> %d) instead of the one that answered", err, before, aCalls.Load())
	}
	// 429 is "did not answer" too
	c2 := NewMempoolSpaceClient(&http.Client{Timeout: 5 * time.Second}, a.URL)
	c2.SetBaseURLs([]string{a.URL, b.URL})
	aStatus.Store(http.StatusTooManyRequests)
	if h, err := c2.GetTipHeight(); err != nil || h != 222 {
		t.Fatalf("after a 429: tip %d err %v, want 222", h, err)
	}
	// a 404 is an answer: no failover, the tx reads as not confirmed
	c3 := NewMempoolSpaceClient(&http.Client{Timeout: 5 * time.Second}, b.URL)
	c3.SetBaseURLs([]string{b.URL, a.URL})
	aBefore := aCalls.Load()
	d, err := c3.GetTxDetails("ab")
	if err != nil || d.Confirmed || aCalls.Load() != aBefore {
		t.Fatalf("404: details %+v err %v, a asked %d times; want unconfirmed, no failover", d, err, aCalls.Load()-aBefore)
	}
	// broadcast fails over too
	aStatus.Store(http.StatusBadGateway)
	c4 := NewMempoolSpaceClient(&http.Client{Timeout: 5 * time.Second}, a.URL)
	c4.SetBaseURLs([]string{a.URL, b.URL})
	if err := c4.PostTx("00"); err != nil {
		t.Fatalf("PostTx after a 502: %v", err)
	}
	// every endpoint down: the last error comes back
	c5 := NewMempoolSpaceClient(&http.Client{Timeout: 5 * time.Second}, a.URL)
	if _, err := c5.GetTipHeight(); err == nil {
		t.Fatal("a single endpoint answering 502 gave no error")
	}
}
