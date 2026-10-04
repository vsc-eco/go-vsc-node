package haf

import (
	"context"
	"os"
	"testing"

	"vsc-node/lib/utils"
	"vsc-node/modules/db/vsc/hive_blocks"
	"vsc-node/modules/hive/streamer"

	"github.com/chebyrash/promise"
	"github.com/stretchr/testify/assert"
)

// TestLiveHafRoundTrip exercises the real client against a HAF database when
// HAF_TEST_URI is set (e.g. postgresql://user@host:5432/haf_block_log). It
// validates the irreversible-view queries, bytea hex encoding, jsonb body
// decoding and op_pos_real assembly against live data.
func TestLiveHafRoundTrip(t *testing.T) {
	uri := os.Getenv("HAF_TEST_URI")
	if uri == "" {
		t.Skip("HAF_TEST_URI not set")
	}

	client, err := NewClient(context.Background(), uri)
	assert.NoError(t, err)
	defer client.Close()

	ctx := context.Background()

	head, err := client.Head(ctx)
	assert.NoError(t, err)
	assert.Greater(t, head, uint64(94_600_000), "HAF should hold mainnet history")

	// block 109857603 contains a gateway savings deposit with a
	// transfer_to_savings_operation and a generated interest_operation vop
	// (verified against account_history_api.get_ops_in_block and psql).
	const probe = uint64(109857603)
	headers, ops, err := client.FetchRange(ctx, probe, probe)
	assert.NoError(t, err)
	assert.Len(t, headers, 1)
	assert.Equal(t, probe, headers[0].Num)
	assert.NotEmpty(t, headers[0].ProducerName)
	assert.Len(t, headers[0].Hash, 20, "block ids are ripemd160 (40 hex chars)")
	assert.Equal(t, "2026-09-12T19:13:36", headers[0].CreatedAt.UTC().Format(timestampFormat))

	blocks, err := AssembleBlocks(headers, ops, []streamer.FilterFunc{vscFilter}, []streamer.VirtualFilterFunc{interestVFilter})
	assert.NoError(t, err)
	assert.Len(t, blocks, 1)
	blk := blocks[0]
	assert.Equal(t, probe, blk.BlockNumber)

	// trx 4 (witness_set_properties) and trx 13 (custom_json +
	// transfer_to_savings + interest vop) are both relevant; trx 13's
	// transaction id is 85199fd6…d097c
	assert.Len(t, blk.Transactions, 2, "both relevant txs are kept")

	tx := blk.Transactions[1]
	assert.Equal(t, 13, tx.Index)
	assert.Equal(t, "85199fd604153b782011479384d788402f3d097c", tx.TransactionID)
	assert.Len(t, tx.Operations, 2, "real ops only; the vop is excluded")
	assert.Equal(t, "custom_json", tx.Operations[0].Type)
	assert.Equal(t, "transfer_to_savings", tx.Operations[1].Type)

	assert.Equal(t, 4, blk.Transactions[0].Index)
	assert.Equal(t, "witness_set_properties", blk.Transactions[0].Operations[0].Type)

	// the interest vop rides along (height is above the claim start)
	assert.Len(t, blk.VirtualOps, 1)
	assert.Equal(t, "interest_operation", blk.VirtualOps[0].Op.Type)
	owner, _ := blk.VirtualOps[0].Op.Value["owner"].(string)
	assert.Equal(t, "vsc.gateway", owner)
	interest, _ := blk.VirtualOps[0].Op.Value["interest"].(map[string]interface{})
	assert.NotNil(t, interest)
	amt, _ := interest["amount"].(string)
	assert.Equal(t, "32446", amt, "NAI asset amounts decode as strings")

	// shim round-trip: StoreBlocks must keep only block_number + timestamp
	store := &shimRecorder{}
	src := newSource(client, store, []streamer.FilterFunc{vscFilter}, nil, 0)
	assert.NoError(t, src.StoreBlocks(head, blk))
	assert.Len(t, store.blocks, 1)
	assert.Equal(t, probe, store.blocks[0].BlockNumber)
	assert.NotEmpty(t, store.blocks[0].Timestamp)
	assert.Empty(t, store.blocks[0].BlockID)
	assert.Empty(t, store.blocks[0].Transactions)

	// GetBlock agrees with the batch fetch
	got, err := src.GetBlock(probe)
	assert.NoError(t, err)
	assert.Equal(t, blk.BlockID, got.BlockID)
	assert.Equal(t, blk.Timestamp, got.Timestamp)

	// empty heights deliver blocks too (contiguous batch)
	batch, err := src.FetchStoredBlocks(head-4, head)
	assert.NoError(t, err)
	assert.Len(t, batch, 5, "every height in range yields a block")
	for i := 1; i < len(batch); i++ {
		assert.Equal(t, batch[i-1].BlockNumber+1, batch[i].BlockNumber, "heights stay contiguous")
	}
}

// shimRecorder captures StoreBlocks calls to assert shim-only persistence.
type shimRecorder struct {
	blocks []hive_blocks.HiveBlock
	head   uint64
}

func (r *shimRecorder) StoreBlocks(headBlock uint64, blocks ...hive_blocks.HiveBlock) error {
	r.head = headBlock
	r.blocks = append(r.blocks, blocks...)
	return nil
}
func (r *shimRecorder) ClearBlocks() error                     { r.blocks = nil; return nil }
func (r *shimRecorder) StoreLastProcessedBlock(uint64) error   { return nil }
func (r *shimRecorder) GetLastProcessedBlock() (uint64, error) { return 0, nil }
func (r *shimRecorder) FetchStoredBlocks(uint64, uint64) ([]hive_blocks.HiveBlock, error) {
	return nil, nil
}
func (r *shimRecorder) ListenToBlockUpdates(ctx context.Context, start uint64, fn func(hive_blocks.HiveBlock, *uint64) error) (context.CancelFunc, <-chan error) {
	return func() {}, nil
}
func (r *shimRecorder) GetHighestBlock() (uint64, error) { return r.head, nil }
func (r *shimRecorder) GetBlock(uint64) (hive_blocks.HiveBlock, error) {
	return hive_blocks.HiveBlock{}, nil
}
func (r *shimRecorder) SetMetadata(doc hive_blocks.Document) error { return nil }
func (r *shimRecorder) GetMetadata() (hive_blocks.Document, error) {
	return hive_blocks.Document{}, nil
}
func (r *shimRecorder) Init() error                  { return nil }
func (r *shimRecorder) Start() *promise.Promise[any] { return utils.PromiseResolve[any](nil) }
func (r *shimRecorder) Stop() error                  { return nil }
