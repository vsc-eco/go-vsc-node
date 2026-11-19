package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// I-34, testnet 11-05 01:28: a SIGTERM was not honored for 75 s (stopped with
// a SIGQUIT; its stacks: every scan worker in net/http under logsAtRetry,
// main in scanBlocksParallel's WaitGroup), because the scan's L1 calls ran on
// context.Background(). Against an endpoint that accepts and never answers,
// a stop now ends the block scan at once (the block fails: scanned again).
func TestI34_AStopEndsAHungBlockScan(t *testing.T) {
	release := make(chan struct{})
	hung := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); hung.Close() })
	for _, c := range []struct {
		name string
		run  func(rpc *ethRPC) error
	}{
		{"block fetch (scanBlockSafe)", func(rpc *ethRPC) error {
			_, err := scanBlockSafe(rpc, 100, []string{"0x0000000000000000000000000000000000000001"}, nil)
			return err
		}},
		{"eth_getLogs (logsAtRetry)", func(rpc *ethRPC) error {
			_, err := logsAtRetry(rpc, `{"fromBlock":"0x64","toBlock":"0x64"}`, 100)
			return err
		}},
	} {
		ctx, cancel := context.WithCancel(context.Background())
		old := shutdownCtx
		shutdownCtx = ctx
		done := make(chan error, 1)
		rpc := newEthRPC(hung.URL + "," + hung.URL)
		go func() { done <- c.run(rpc) }()
		time.Sleep(200 * time.Millisecond)
		cancel()
		select {
		case err := <-done:
			if err == nil {
				t.Fatalf("%s: a scan stopped mid-call reported success", c.name)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%s: did not end within 3 s of the stop (it waits on the hung endpoint)", c.name)
		}
		shutdownCtx = old
	}
}
