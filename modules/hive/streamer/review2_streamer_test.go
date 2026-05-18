package streamer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vsc-eco/hivego"
)

// errVopClient: GetBlockRange/GetDynamicGlobalProps are never reached by this
// test; FetchVirtualOps always errors.
type errVopClient struct{}

func (errVopClient) GetDynamicGlobalProps() ([]byte, error) { return nil, nil }
func (errVopClient) GetBlockRange(int, int) ([]hivego.Block, error) {
	return nil, nil
}
func (errVopClient) FetchVirtualOps(int, bool, bool) ([]hivego.VirtualOp, error) {
	return nil, errors.New("rpc down")
}

// alwaysVopErrClient returns one block per range (so the loop has work)
// whose only op needs virtual ops, and always fails FetchVirtualOps.
type alwaysVopErrClient struct{ rangeCalls int64 }

func (c *alwaysVopErrClient) GetDynamicGlobalProps() ([]byte, error) { return nil, nil }
func (c *alwaysVopErrClient) GetBlockRange(start, count int) ([]hivego.Block, error) {
	atomic.AddInt64(&c.rangeCalls, 1)
	return []hivego.Block{{
		BlockNumber:    start,
		BlockID:        "0000000000000000000000000000000000000000",
		Timestamp:      "2026-05-18T00:00:00",
		TransactionIds: []string{"tx0"},
		Transactions: []hivego.Transaction{
			{Operations: []hivego.Operation{{Type: "custom_json_operation"}}},
		},
	}}, nil
}
func (c *alwaysVopErrClient) FetchVirtualOps(int, bool, bool) ([]hivego.VirtualOp, error) {
	return nil, errors.New("rpc down")
}

// review2 HIGH #25 (follow-up to milo's review) — the caller advanced
// *s.startBlock BEFORE storeBlocks ran in a detached goroutine, so an
// in-process storeBlocks failure permanently skipped the batch; only a
// restart (Init re-reads GetHighestBlock) recovered it. The cursor must
// not move until the batch is durably stored, so a failing batch is
// retried in-process rather than skipped.
func TestStartBlockNotAdvancedOnStoreFailure(t *testing.T) {
	client := &alwaysVopErrClient{}
	ctx, cancel := context.WithCancel(context.Background())
	start := uint64(100)
	s := &Streamer{
		client:     client,
		hiveBlocks: nil, // fail-closed never reaches the store
		startBlock: &start,
		ctx:        ctx,
		cancel:     cancel,
		filters: []FilterFunc{
			func(_ hivego.Operation, bp *BlockParams) bool {
				bp.NeedsVirtualOps = true
				return true
			},
		},
		headHeight:     1_000_000,
		hasFetchedHead: true,
	}

	done := make(chan struct{})
	go func() { defer close(done); s.streamBlocks() }()

	// Let the loop fetch+attempt-store several batches.
	time.Sleep(150 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("streamBlocks did not exit after ctx cancel")
	}

	if atomic.LoadInt64(&client.rangeCalls) == 0 {
		t.Fatal("test sanity: loop never fetched a batch")
	}
	s.mtx.Lock()
	got := *s.startBlock
	s.mtx.Unlock()
	if got != start {
		t.Fatalf("review2 #25: cursor advanced to %d despite every storeBlocks failing — failed batch was skipped, not retried", got)
	}
}

// review2 HIGH #25 — storeBlocks did `virtualOps, _ := s.client.FetchVirtualOps(...)`,
// discarding the error: on an RPC failure a block that NeedsVirtualOps was
// stored as if it had none (silently dropping deposits/payouts), then control
// flowed on to s.hiveBlocks.StoreBlocks.
//
// This test drives the real storeBlocks with a client that errors on
// FetchVirtualOps and a filter that marks the block as needing virtual ops,
// and a nil hiveBlocks (never reached if we fail closed).
//
//   - fix/review2: storeBlocks returns BEFORE the store, error wraps the
//     FetchVirtualOps failure → PASS (GREEN).
//   - #170 baseline: the error is swallowed and control proceeds to
//     s.hiveBlocks.StoreBlocks(nil) → nil-pointer panic (caught here) — i.e.
//     not "failed closed" → FAIL (RED), demonstrating the swallow.
func TestStoreBlocksFailsClosedOnFetchVirtualOpsError(t *testing.T) {
	s := &Streamer{
		client: errVopClient{},
		filters: []FilterFunc{
			func(_ hivego.Operation, ctx *BlockParams) bool {
				ctx.NeedsVirtualOps = true
				return true
			},
		},
		// hiveBlocks deliberately nil: a correct fail-closed path never
		// reaches the store.
	}

	blk := hivego.Block{
		BlockNumber:    42,
		BlockID:        "0000002a00000000000000000000000000000000",
		Timestamp:      "2026-05-16T00:00:00",
		TransactionIds: []string{"tx0"},
		Transactions: []hivego.Transaction{
			{Operations: []hivego.Operation{{Type: "custom_json_operation"}}},
		},
	}

	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("storeBlocks panicked instead of failing closed (error swallowed, then store on nil): %v", r)
			}
		}()
		err = s.storeBlocks([]hivego.Block{blk})
	}()

	if err == nil {
		t.Fatalf("review2 #25: storeBlocks returned nil — FetchVirtualOps error was swallowed and an incomplete block would be stored")
	}
	if !strings.Contains(err.Error(), "FetchVirtualOps") {
		t.Fatalf("review2 #25: storeBlocks must fail closed wrapping the FetchVirtualOps error; got: %v", err)
	}
}
