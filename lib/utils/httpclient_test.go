package utils

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// freezeRelay forwards TCP to upstream; freeze makes every connection open
// now go silent (bytes in are dropped, none come out; it is never closed), as
// an established connection does when a NAT forgets it. New connections pass.
type freezeRelay struct {
	ln    net.Listener
	mu    sync.Mutex
	conns []*atomic.Bool
}

func newFreezeRelay(t *testing.T, upstream string) *freezeRelay {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &freezeRelay{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			u, err := net.Dial("tcp", upstream)
			if err != nil {
				c.Close()
				continue
			}
			frozen := new(atomic.Bool)
			r.mu.Lock()
			r.conns = append(r.conns, frozen)
			r.mu.Unlock()
			pipe := func(dst, src net.Conn) {
				buf := make([]byte, 32<<10)
				for {
					n, err := src.Read(buf)
					if err != nil {
						return
					}
					if !frozen.Load() {
						if _, err := dst.Write(buf[:n]); err != nil {
							return
						}
					}
				}
			}
			go pipe(u, c)
			go pipe(c, u)
		}
	}()
	return r
}

func (r *freezeRelay) freeze() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, f := range r.conns {
		f.Store(true)
	}
}

func TestI35_ASilentHTTP2ConnectionIsReplaced(t *testing.T) {
	defer func(a, b time.Duration) { HTTP2PingAfter, HTTP2PingTimeout = a, b }(HTTP2PingAfter, HTTP2PingTimeout)
	HTTP2PingAfter, HTTP2PingTimeout = 300*time.Millisecond, 300*time.Millisecond

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`))
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	relay := newFreezeRelay(t, srv.Listener.Addr().String())

	c := NewHTTPClient(time.Second)
	c.Transport.(*http.Transport).TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	post := func() error {
		resp, err := c.Post("https://"+relay.ln.Addr().String(), "application/json", strings.NewReader(`{}`))
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		if resp.ProtoMajor != 2 {
			t.Fatalf("the test needs HTTP/2, got %s", resp.Proto)
		}
		return nil
	}
	if err := post(); err != nil {
		t.Fatal(err)
	}
	relay.freeze()
	deadline := time.Now().Add(6 * time.Second)
	for fails := 0; ; fails++ {
		if err := post(); err == nil {
			t.Logf("a new connection served the call after %d failed call(s)", fails)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("calls still fail 6 s after the connection went silent (%d failures): the dead HTTP/2 connection is kept", fails)
		}
	}
}
