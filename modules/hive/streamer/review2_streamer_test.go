package streamer

import (
	"errors"
	"fmt"
	"strings"
	"testing"

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
