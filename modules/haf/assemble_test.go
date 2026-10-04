package haf

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/db/vsc/hive_blocks"
	"vsc-node/modules/hive/streamer"

	"github.com/stretchr/testify/assert"
	"github.com/vsc-eco/hivego"
	"go.mongodb.org/mongo-driver/mongo"
)

// stubFetcher serves canned header/op rows for [start, end].
type stubFetcher struct {
	headers []BlockRow
	ops     []OpRow
	head    uint64
	calls   int
}

func (s *stubFetcher) Head(ctx context.Context) (uint64, error) {
	return s.head, nil
}

func (s *stubFetcher) FetchRange(ctx context.Context, start, end uint64) ([]BlockRow, []OpRow, error) {
	s.calls++
	var headers []BlockRow
	for _, h := range s.headers {
		if h.Num >= start && h.Num <= end {
			headers = append(headers, h)
		}
	}
	var ops []OpRow
	for _, o := range s.ops {
		if o.BlockNum >= start && o.BlockNum <= end {
			ops = append(ops, o)
		}
	}
	return headers, ops, nil
}

func mustBody(t *testing.T, typ string, value map[string]interface{}) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]interface{}{"type": typ, "value": value})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func header(num uint64) BlockRow {
	return BlockRow{
		Num:          num,
		Hash:         []byte{0x01, 0x02},
		CreatedAt:    time.Date(2026, 9, 12, 19, 13, 36, 0, time.UTC),
		MerkleRoot:   []byte{0x03, 0x04},
		ProducerName: "abit",
	}
}

// vscFilter mirrors cmd/vsc-node's production relevance filter shape.
var vscFilter = func(op hivego.Operation, ctx *streamer.BlockParams) bool {
	switch op.Type {
	case "custom_json":
		id, _ := op.Value["id"].(string)
		return len(id) >= 4 && id[:4] == "vsc."
	case "account_update", "account_update2", "feed_publish", "witness_set_properties":
		return true
	case "transfer", "transfer_to_savings", "transfer_from_savings":
		to, _ := op.Value["to"].(string)
		from, _ := op.Value["from"].(string)
		isVsc := (len(to) >= 4 && to[:4] == "vsc.") || (len(from) >= 4 && from[:4] == "vsc.")
		if isVsc && ctx.BlockHeight > 95900000 &&
			(op.Type == "transfer_to_savings" || op.Type == "transfer_from_savings") {
			ctx.NeedsVirtualOps = true
		}
		return isVsc
	}
	return false
}

var interestVFilter = func(op hivego.VirtualOp) bool {
	return op.Op.Type == "interest_operation"
}

func TestAssembleBlocksOpPosRealOrdering(t *testing.T) {
	// rows arrive in fetcher order (block_num, trx_in_block, op_pos_real):
	// tx 7 real ops at 0 (custom_json) then 1 (transfer), with the virtual
	// op last since op_pos_real NULL sorts to the tail.
	ops := []OpRow{
		{BlockNum: 10, TrxInBlock: 7, OpPosReal: int32Ptr(0), TrxHash: []byte{0xaa}, Body: mustBody(t, "custom_json_operation", map[string]interface{}{"id": "vsc.x", "json": "{}"})},
		{BlockNum: 10, TrxInBlock: 7, OpPosReal: int32Ptr(1), TrxHash: []byte{0xaa}, Body: mustBody(t, "transfer_operation", map[string]interface{}{"to": "vsc.gateway", "from": "a", "memo": "m"})},
		{BlockNum: 10, TrxInBlock: 7, OpPosReal: nil, TrxHash: []byte{0xaa}, Body: mustBody(t, "interest_operation", map[string]interface{}{"owner": "vsc.gateway"})},
	}
	blocks, err := AssembleBlocks([]BlockRow{header(10)}, ops, []streamer.FilterFunc{vscFilter}, nil)
	assert.NoError(t, err)
	assert.Len(t, blocks, 1)
	blk := blocks[0]
	assert.Equal(t, uint64(10), blk.BlockNumber)
	assert.Equal(t, "0102", blk.BlockID)
	assert.Equal(t, "0304", blk.MerkleRoot)
	assert.Equal(t, "abit", blk.Witness)
	assert.Equal(t, "2026-09-12T19:13:36", blk.Timestamp)

	assert.Len(t, blk.Transactions, 1)
	tx := blk.Transactions[0]
	assert.Equal(t, 7, tx.Index)
	assert.Equal(t, "aa", tx.TransactionID)
	assert.Len(t, tx.Operations, 2)
	// op_pos_real ordering: 0 (custom_json) before 1 (transfer); the vop is excluded
	assert.Equal(t, "custom_json", tx.Operations[0].Type)
	assert.Equal(t, "transfer", tx.Operations[1].Type)
}

func TestAssembleBlocksWholeTxInclusion(t *testing.T) {
	// only the second op of tx 3 is relevant; the whole tx must be kept
	ops := []OpRow{
		{BlockNum: 11, TrxInBlock: 3, OpPosReal: int32Ptr(0), TrxHash: []byte{0xbb}, Body: mustBody(t, "vote_operation", map[string]interface{}{"voter": "x"})},
		{BlockNum: 11, TrxInBlock: 3, OpPosReal: int32Ptr(1), TrxHash: []byte{0xbb}, Body: mustBody(t, "custom_json_operation", map[string]interface{}{"id": "vsc.deposit", "json": "{}"})},
	}
	// and an irrelevant tx that must be dropped
	ops = append(ops, OpRow{BlockNum: 11, TrxInBlock: 4, OpPosReal: int32Ptr(0), TrxHash: []byte{0xcc}, Body: mustBody(t, "vote_operation", map[string]interface{}{"voter": "y"})})

	blocks, err := AssembleBlocks([]BlockRow{header(11)}, ops, []streamer.FilterFunc{vscFilter}, nil)
	assert.NoError(t, err)
	blk := blocks[0]
	assert.Len(t, blk.Transactions, 1)
	assert.Equal(t, 3, blk.Transactions[0].Index)
	assert.Len(t, blk.Transactions[0].Operations, 2, "all ops of an included tx are kept")
	assert.Equal(t, "vote", blk.Transactions[0].Operations[0].Type)
}

func TestAssembleBlocksVirtualOpGating(t *testing.T) {
	interestOp := OpRow{BlockNum: 12, TrxInBlock: 9, OpPosReal: nil, TrxHash: []byte{0xdd}, Body: mustBody(t, "interest_operation", map[string]interface{}{"owner": "vsc.gateway"})}
	savingsTx := OpRow{BlockNum: 12, TrxInBlock: 9, OpPosReal: int32Ptr(0), TrxHash: []byte{0xdd}, Body: mustBody(t, "transfer_to_savings_operation", map[string]interface{}{"to": "vsc.gateway", "from": "a"})}

	// below MAINNET_CLAIM_START-equivalent height: no virtual-op capture
	blocks, err := AssembleBlocks([]BlockRow{header(12)}, []OpRow{savingsTx, interestOp},
		[]streamer.FilterFunc{vscFilter}, []streamer.VirtualFilterFunc{interestVFilter})
	assert.NoError(t, err)
	assert.Len(t, blocks[0].VirtualOps, 0, "vops not captured before the claim start height")

	// above it: the interest vop rides along with the savings tx
	h := header(12)
	h.Num = 95900001
	savingsTx.BlockNum = 95900001
	interestOp.BlockNum = 95900001
	blocks, err = AssembleBlocks([]BlockRow{h}, []OpRow{savingsTx, interestOp},
		[]streamer.FilterFunc{vscFilter}, []streamer.VirtualFilterFunc{interestVFilter})
	assert.NoError(t, err)
	assert.Len(t, blocks[0].VirtualOps, 1)
	assert.Equal(t, "interest_operation", blocks[0].VirtualOps[0].Op.Type)
	assert.Equal(t, "dd", blocks[0].VirtualOps[0].TrxId)

	// vops kept only if vFilters accept them
	blocks, err = AssembleBlocks([]BlockRow{h}, []OpRow{savingsTx, interestOp},
		[]streamer.FilterFunc{vscFilter}, []streamer.VirtualFilterFunc{func(op hivego.VirtualOp) bool { return false }})
	assert.NoError(t, err)
	assert.Len(t, blocks[0].VirtualOps, 0)
}

func TestAssembleBlocksEmptyHeightsStillYieldBlocks(t *testing.T) {
	blocks, err := AssembleBlocks([]BlockRow{header(20), header(21)}, nil, []streamer.FilterFunc{vscFilter}, nil)
	assert.NoError(t, err)
	assert.Len(t, blocks, 2, "empty heights must still produce blocks for per-height ticks")
	assert.Len(t, blocks[0].Transactions, 0)
}

func TestStoreBlocksShimsRelevantOnly(t *testing.T) {
	store := &test_utils.MockHiveBlockDb{}
	src := newSource(&stubFetcher{}, store, nil, nil, 0)

	err := src.StoreBlocks(100,
		hive_blocks.HiveBlock{BlockNumber: 1, Timestamp: "2026-01-01T00:00:00"}, // empty: not stored
		hive_blocks.HiveBlock{BlockNumber: 2, Timestamp: "2026-01-01T00:00:03", Transactions: []hive_blocks.Tx{{Index: 0}}},
	)
	assert.NoError(t, err)
	assert.Len(t, store.Blocks, 1, "blocks without relevant transactions are not stored")
	assert.Equal(t, uint64(2), store.Blocks[0].BlockNumber)
	assert.Equal(t, "2026-01-01T00:00:03", store.Blocks[0].Timestamp)
	// shim-only: no txs, ids or ops persisted
	assert.Len(t, store.Blocks[0].Transactions, 0)
	assert.Empty(t, store.Blocks[0].BlockID)
	assert.Empty(t, store.Blocks[0].VirtualOps)

	// a batch with no relevant blocks keeps the head height fresh without writes
	assert.NoError(t, src.StoreBlocks(101, hive_blocks.HiveBlock{BlockNumber: 3, Timestamp: "t"}))
	assert.Len(t, store.Blocks, 1)
	if assert.NotNil(t, store.Metadata.HeadHeight) {
		assert.Equal(t, uint64(101), *store.Metadata.HeadHeight)
	}
}

func TestGetBlockStartFloorAndMissing(t *testing.T) {
	f := &stubFetcher{
		headers: []BlockRow{header(50)},
		head:    50,
	}
	store := &test_utils.MockHiveBlockDb{}
	src := newSource(f, store, []streamer.FilterFunc{vscFilter}, nil, 40)

	// below the ingestion floor: same answer as the API-mode store
	_, err := src.GetBlock(39)
	assert.ErrorIs(t, err, mongo.ErrNoDocuments)

	// present
	blk, err := src.GetBlock(50)
	assert.NoError(t, err)
	assert.Equal(t, uint64(50), blk.BlockNumber)

	// absent but in range
	_, err = src.GetBlock(45)
	assert.ErrorIs(t, err, mongo.ErrNoDocuments)
}

func TestFetchStoredBlocksClampsToFloor(t *testing.T) {
	f := &stubFetcher{
		headers: []BlockRow{header(40), header(41)},
		head:    41,
	}
	store := &test_utils.MockHiveBlockDb{}
	src := newSource(f, store, nil, nil, 40)

	blocks, err := src.FetchStoredBlocks(10, 41)
	assert.NoError(t, err)
	assert.Len(t, blocks, 2, "range is clamped to the ingestion floor")

	blocks, err = src.FetchStoredBlocks(1, 39)
	assert.NoError(t, err)
	assert.Len(t, blocks, 0, "range entirely below the floor returns nothing")
}

func TestListenDeliversEveryHeight(t *testing.T) {
	f := &stubFetcher{
		headers: []BlockRow{header(1), header(2), header(3)},
		ops: []OpRow{
			{BlockNum: 2, TrxInBlock: 0, OpPosReal: int32Ptr(0), TrxHash: []byte{0x01}, Body: mustBody(t, "custom_json_operation", map[string]interface{}{"id": "vsc.a", "json": "{}"})},
		},
		head: 3,
	}
	store := &test_utils.MockHiveBlockDb{}
	src := newSource(f, store, []streamer.FilterFunc{vscFilter}, nil, 0)

	got := []uint64{}
	done := make(chan struct{})
	_, errs := src.ListenToBlockUpdates(context.Background(), 1, func(b hive_blocks.HiveBlock, head *uint64) error {
		got = append(got, b.BlockNumber)
		if len(got) == 3 {
			close(done)
		}
		return nil
	})
	select {
	case <-done:
	case <-errs:
	case <-time.After(3 * time.Second):
		t.Fatal("listener never delivered all blocks")
	}

	assert.Equal(t, []uint64{1, 2, 3}, got, "every height is delivered, empty ones too")
	assert.Len(t, store.Blocks, 1, "only the relevant height is shimmed")
	assert.Equal(t, uint64(2), store.Blocks[0].BlockNumber)
}

func int32Ptr(v int32) *int32 {
	return &v
}
