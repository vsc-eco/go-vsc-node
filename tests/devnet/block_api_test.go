package devnet

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The bodies are what drone and PostgREST answered during a real H-37 boot hang
// (null result, PGRST202) and what a healthy stack answers.
func TestBlockAPIServesRequiresARealBlock(t *testing.T) {
	cases := map[string]struct {
		body string
		ok   bool
	}{
		"block returned":  {`{"jsonrpc":"2.0","result":{"blocks":[{"previous":"0000000000000000000000000000000000000000","witness":"initminer"}]}}`, true},
		"null result":     {`{"id":1,"jsonrpc":"2.0","result":null}`, false},
		"empty range":     {`{"jsonrpc":"2.0","result":{"blocks":[]}}`, false},
		"stale schema":    {`{"code":"PGRST202","details":"Searched for the function hafah_endpoints.home","hint":null,"message":"Could not find the function hafah_endpoints.home(id, jsonrpc, method, params) in the schema cache"}`, false},
		"rpc error":       {`{"jsonrpc":"2.0","error":{"code":-32003,"message":"backend down"}}`, false},
		"not json at all": {`<html>502 Bad Gateway</html>`, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()
			err := blockAPIServes(context.Background(), &http.Client{Timeout: time.Second}, srv.URL)
			if c.ok && err != nil {
				t.Fatalf("healthy block API rejected: %v", err)
			}
			if !c.ok && err == nil {
				t.Fatalf("broken block API accepted: %s", c.body)
			}
		})
	}
}
