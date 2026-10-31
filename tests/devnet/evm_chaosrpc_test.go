//go:build evm_devnet

package devnet

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// The chaos RPC injects each fault, keeps request ids, never alters an answer
// it forwards, and answers a stale head only with an older real answer.
func TestChaosRPC_FaultsAreRealAndAnswersUntouched(t *testing.T) {
	var head, upCalls atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		upCalls.Add(1)
		result := `"echo-` + req.Method + `"`
		if req.Method == "eth_blockNumber" {
			result = `"0x` + strconv.FormatInt(head.Add(1), 16) + `"`
		}
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, req.ID, result)
	}))
	defer up.Close()
	cfg := MainnetWeather(7)
	cfg.SlowMin, cfg.SlowMax, cfg.StaleAge = time.Millisecond, 3*time.Millisecond, 200*time.Millisecond
	c, err := StartChaosRPC(up.URL, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Stop()

	call := func(id int, method string) (int, map[string]any) {
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"%s","params":[]}`, id, method)
		resp, err := http.Post(c.URL, "application/json", bytes.NewBufferString(body))
		if err != nil {
			return -1, nil // a dropped connection
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	var maxSeen int64
	for i := 1; i <= 2000; i++ {
		method := "eth_getBalance"
		if i%2 == 0 {
			method = "eth_blockNumber"
		}
		status, out := call(i, method)
		if status != http.StatusOK {
			continue
		}
		if id, _ := out["id"].(float64); int(id) != i {
			t.Fatalf("call %d answered with id %v", i, out["id"])
		}
		if method == "eth_getBalance" {
			if out["result"] != "echo-eth_getBalance" {
				t.Fatalf("a forwarded answer was altered: %v", out)
			}
			continue
		}
		n, err := strconv.ParseInt(out["result"].(string)[2:], 16, 64)
		if err != nil || n < 1 || n > head.Load() {
			t.Fatalf("head answer %v is not a real head (upstream at %d)", out["result"], head.Load())
		}
		if n > maxSeen {
			maxSeen = n
		}
		if i%50 == 0 {
			time.Sleep(20 * time.Millisecond) // wall: let answers age into the proxy's real-time stale window
		}
	}
	got := c.Counts()
	for _, f := range []string{"429", "502", "drop", "lost", "slow", "stale"} {
		if got[f] == 0 {
			t.Fatalf("no %s fault in 2000 calls: %s", f, c)
		}
	}
	if got["calls"] != 2000 {
		t.Fatalf("calls counted %d, want 2000", got["calls"])
	}
	// Refused, dropped and stale calls never reach the node; lost ones did.
	if want := int64(2000 - got["429"] - got["502"] - got["drop"] - got["stale"]); upCalls.Load() != want {
		t.Fatalf("upstream ran %d calls, want %d (lost calls are run, refused ones not)", upCalls.Load(), want)
	}
	c.SetFaults(false)
	for i := 0; i < 50; i++ {
		if status, _ := call(5000+i, "eth_getBalance"); status != http.StatusOK {
			t.Fatalf("with faults off a call failed: %d", status)
		}
	}
	t.Logf("2000 calls: %s", c)
}
