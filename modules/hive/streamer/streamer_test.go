package streamer_test

// ===== NOTE =====
// despite this streamer relying on live RPC calls, we make the test
// deterministic by mocking the block client service
// ===== NOTE =====

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/db/vsc/hive_blocks"
	"vsc-node/modules/hive/streamer"

	"vsc-node/lib/test_utils"

	rand "math/rand/v2"

	"github.com/stretchr/testify/assert"
	"github.com/vsc-eco/hivego"
)

// ===== constants =====

const (
	dummyBlockHead = 244_842_084
)

// ===== init =====

func init() {
	// ensure we have consistent testing settings & env
	streamer.AcceptableBlockLag = 0
	streamer.BlockBatchSize = 100
	streamer.DefaultBlockStart = 81614028
	streamer.HeadBlockCheckPollIntervalBeforeFirstUpdate = time.Millisecond * 200
	streamer.MinTimeBetweenBlockBatchFetches = time.Millisecond * 200
	streamer.DbPollInterval = time.Millisecond * 400
}

// ===== test utils =====

func seedBlockData(t *testing.T, hiveBlocks hive_blocks.HiveBlocks, n uint64) {

	for i := uint64(1); i <= n; i++ {
		// a dummy hiveblock
		block := hive_blocks.HiveBlock{
			BlockNumber: i,
			BlockID:     fmt.Sprintf("some-block-id-%d", i),
			Timestamp:   "2024-01-01T00:00:00",
			MerkleRoot:  fmt.Sprintf("some-merkle-root-%d", i),
			Transactions: []hive_blocks.Tx{
				{
					TransactionID: fmt.Sprintf("some-tx-id-%d", i),
					Operations: []hivego.Operation{
						{
							Value: map[string]interface{}{
								"amount": "1 HIVE",
							},
							Type: "custom_json",
						},
					},
				},
			},
		}

		// store block
		assert.NoError(t, hiveBlocks.StoreBlocks(n, block))
	}
}

// ===== mock hive block client service =====

type MockBlockClient struct{}

var _ streamer.BlockClient = &MockBlockClient{}

func (m *MockBlockClient) GetDynamicGlobalProps() ([]byte, error) {
	// await for random duration to simulate real-world scenario
	//
	// to contribute to innate thread randomness since these tests because of that can't be 100% deterministic anyway
	time.Sleep(time.Millisecond * time.Duration(10+rand.IntN(90)))
	props, _ := json.Marshal(map[string]interface{}{
		"head_block_number": float64(dummyBlockHead), // dummy head, whatever it may be
	})
	return props, nil
}

func (m *MockBlockClient) GetBlockRange(startBlock, count int) ([]hivego.Block, error) {
	// await for random duration to simulate real-world scenario
	//
	// to contribute to innate thread randomness since these tests because of that can't be 100% deterministic anyway
	blockChannel := make([]hivego.Block, count)
	for i := int(0); i < count; i++ {
		blockNumber := startBlock + i
		blockChannel[i] = hivego.Block{
			Timestamp:             time.Now().UTC().Format(time.RFC3339),
			TransactionMerkleRoot: fmt.Sprintf("fake-merkle-root-%d", blockNumber),
			TransactionIds:        []string{fmt.Sprintf("fake-tx-id-%d", blockNumber)},
			BlockNumber:           blockNumber,
			BlockID:               fmt.Sprintf("fake-block-id-%d", blockNumber),
			Transactions: []hivego.Transaction{
				{
					RefBlockNum: uint16(blockNumber),
					Operations: []hivego.Operation{
						{
							Type:  "transfer_operation",
							Value: map[string]interface{}{"amount": "1 HIVE"},
						},
					},
				},
			},
		}
	}

	return blockChannel, nil
}

func (m *MockBlockClient) FetchVirtualOps(
	block int,
	onlyVirtual bool,
	includeReversible bool,
) ([]hivego.VirtualOp, error) {

	return nil, nil
}

// ===== tests =====

func TestFetchStoreBlocks(t *testing.T) {
	// hive mocks db
	mockHiveBlocks := &test_utils.MockHiveBlockDb{}

	startBlock := uint64(0)
	blockClient := &MockBlockClient{}
	s := streamer.NewStreamer(
		blockClient,
		mockHiveBlocks,
		[]streamer.FilterFunc{func(tx hivego.Operation, blockParams *streamer.BlockParams) bool {
			// s := streamer.NewStreamer(&MockBlockClient{}, mockHiveBlocks, []streamer.FilterFunc{func(tx hivego.Operation) bool {
			return true
		}},
		nil,
		nil,
	)

	var totalBlksReceived atomic.Int64

	process := func(block hive_blocks.HiveBlock, headHeigt *uint64) {
		totalBlksReceived.Add(1)
	}

	sr := streamer.NewStreamReader(mockHiveBlocks, process, nil, startBlock)
	agg := aggregate.New([]aggregate.Plugin{
		mockHiveBlocks,
		s,
		sr,
	})

	test_utils.RunPlugin(t, agg)

	assert.Eventually(t, func() bool {
		return totalBlksReceived.Load() > 0
	}, 3*time.Second, 10*time.Millisecond)
}

func TestStartBlock(t *testing.T) {

	// hive blocks
	mockHiveBlocks := &test_utils.MockHiveBlockDb{}

	test_utils.RunPlugin(t, mockHiveBlocks)

	s := streamer.NewStreamer(&MockBlockClient{}, mockHiveBlocks, nil, nil, nil)
	assert.NoError(t, s.Init())

	// should default to our default if we don't specify and instead input nil
	assert.Equal(t, streamer.DefaultBlockStart, s.StartBlock())

	s = streamer.NewStreamer(&MockBlockClient{}, mockHiveBlocks, nil, nil, &[]uint64{99}[0])
	assert.NoError(t, s.Init())

	// should be the value we input
	assert.Equal(t, uint64(99), s.StartBlock())
}

func TestIntensePolling(t *testing.T) {
	// hive blocks
	mockHiveBlocks := &test_utils.MockHiveBlockDb{}

	s := streamer.NewStreamer(&MockBlockClient{}, mockHiveBlocks, nil, nil, nil)
	var seenMtx sync.Mutex
	seenBlocks := make(map[uint64]int)

	sr := streamer.NewStreamReader(mockHiveBlocks, func(block hive_blocks.HiveBlock, headHeigt *uint64) {
		seenMtx.Lock()
		seenBlocks[block.BlockNumber]++
		seenMtx.Unlock()
	}, nil)

	agg := aggregate.New([]aggregate.Plugin{
		mockHiveBlocks,
		s,
		sr,
	})

	test_utils.RunPlugin(t, agg)

	time.Sleep(3 * time.Second)

	// ensure in entire map, no dupes!
	seenMtx.Lock()
	defer seenMtx.Unlock()
	for _, v := range seenBlocks {
		assert.Equal(t, 1, v)
	}
}

func TestStreamFilter(t *testing.T) {
	// hive blocks
	mockHiveBlocks := &test_utils.MockHiveBlockDb{}

	filter := func(op hivego.Operation, blockParams *streamer.BlockParams) bool {
		// filter everything!
		return op.Value["amount"] != "1 HIVE"
	}

	s := streamer.NewStreamer(&MockBlockClient{}, mockHiveBlocks, []streamer.FilterFunc{filter}, nil, nil)

	var txsReceived atomic.Int64

	process := func(block hive_blocks.HiveBlock, headHeigt *uint64) {
		if len(block.Timestamp) > 0 {
			txsReceived.Add(int64(len(block.Transactions)))
		}
	}

	sr := streamer.NewStreamReader(mockHiveBlocks, process, nil)

	agg := aggregate.New([]aggregate.Plugin{
		mockHiveBlocks,
		s,
		sr,
	})

	test_utils.RunPlugin(t, agg)

	time.Sleep(3 * time.Second)

	assert.Equal(t, int64(0), txsReceived.Load())
}

func TestPersistingBlocksStored(t *testing.T) {

	// hive blocks
	mockHiveBlocks := &test_utils.MockHiveBlockDb{}

	// start at 0
	s := streamer.NewStreamer(&MockBlockClient{}, mockHiveBlocks, nil, nil, nil)

	agg := aggregate.New([]aggregate.Plugin{
		mockHiveBlocks,
		s,
	})

	test_utils.RunPlugin(t, agg)

	time.Sleep(3 * time.Second)

	gotToBlock, err := mockHiveBlocks.GetHighestBlock()
	assert.NoError(t, err)

	assert.Greater(t, gotToBlock, uint64(0))

	s = streamer.NewStreamer(&MockBlockClient{}, mockHiveBlocks, nil, nil, nil)

	test_utils.RunPlugin(t, s)

	time.Sleep(3 * time.Second)

	gotToBlockTry2, err := mockHiveBlocks.GetHighestBlock()
	assert.NoError(t, err)

	assert.Greater(t, gotToBlockTry2, streamer.DefaultBlockStart)
	assert.Greater(t, gotToBlockTry2, gotToBlock)
}

func TestPersistingBlocksProcessed(t *testing.T) {
	// hive blocks
	mockHiveBlocks := &test_utils.MockHiveBlockDb{}

	// start at default
	s := streamer.NewStreamer(&MockBlockClient{}, mockHiveBlocks, nil, nil, nil)

	agg := aggregate.New([]aggregate.Plugin{
		mockHiveBlocks,
		s,
	})

	test_utils.RunPlugin(t, agg)

	var lastProcessedBlk atomic.Uint64

	sr := streamer.NewStreamReader(mockHiveBlocks, func(block hive_blocks.HiveBlock, headHeigt *uint64) {
		lastProcessedBlk.Store(block.BlockNumber)
	}, nil)
	assert.NoError(t, sr.Init())
	go func() {
		_, err := sr.Start().Await(context.Background())
		assert.NoError(t, err)
	}()

	time.Sleep(2 * time.Second)

	assert.Greater(t, lastProcessedBlk.Load(), streamer.DefaultBlockStart)

	s.Pause()
	assert.NoError(t, sr.Stop())

	newLastProcessedBlk, err := mockHiveBlocks.GetLastProcessedBlock()
	assert.NoError(t, err)

	assert.Greater(t, newLastProcessedBlk, streamer.DefaultBlockStart)
	assert.Equal(t, lastProcessedBlk.Load(), newLastProcessedBlk)

	assert.NoError(t, sr.Stop())

	var resumedLastProcessedBlk atomic.Uint64

	s.Resume()

	// redefine stream reader and see if it picks up where it left off
	sr = streamer.NewStreamReader(mockHiveBlocks, func(block hive_blocks.HiveBlock, headHeigt *uint64) {
		resumedLastProcessedBlk.Store(block.BlockNumber)
	}, nil)

	test_utils.RunPlugin(t, sr)

	time.Sleep(2 * time.Second)

	assert.Greater(t, resumedLastProcessedBlk.Load(), streamer.DefaultBlockStart)
	assert.Greater(t, resumedLastProcessedBlk.Load(), newLastProcessedBlk)
}

func TestBlockProcessing(t *testing.T) {
	// hive blocks
	mockHiveBlocks := &test_utils.MockHiveBlockDb{}

	test_utils.RunPlugin(t, mockHiveBlocks)

	// seed
	seedBlockData(t, mockHiveBlocks, 10)

	var totalSeenBlocks atomic.Int64

	sr := streamer.NewStreamReader(mockHiveBlocks, func(block hive_blocks.HiveBlock, headHeigt *uint64) {
		totalSeenBlocks.Add(1)
	}, nil, 0)

	test_utils.RunPlugin(t, sr)

	time.Sleep(3 * time.Second)

	assert.Equal(t, int64(10), totalSeenBlocks.Load())
}

// func TestStreamReaderPauseResumeStop(t *testing.T) {
// 	// hive blocks
// 	mockHiveBlocks := &test_utils.MockHiveBlockDb{}

// 	test_utils.RunPlugin(t, mockHiveBlocks)

// 	// seed
// 	seedBlockData(t, mockHiveBlocks, 10)

// 	totalSeenBlocks := 0

// 	sr := streamer.NewStreamReader(mockHiveBlocks, func(block hive_blocks.HiveBlock, headHeigt *uint64) {
// 		totalSeenBlocks++
// 	}, 0)
// 	assert.NoError(t, sr.Init())
// 	go func() {
// 		_, err := sr.Start().Await(context.Background())
// 		assert.NoError(t, err)
// 	}()

// 	time.Sleep(3 * time.Second)

// 	seenBlocksBeforePause := totalSeenBlocks

// 	sr.Pause()

// 	time.Sleep(1 * time.Second)

// 	assert.Equal(t, seenBlocksBeforePause, totalSeenBlocks)

// 	// resume
// 	sr.Resume()

// 	// seed
// 	seedBlockData(t, mockHiveBlocks, 20)

// 	time.Sleep(1 * time.Second)

// 	assert.Greater(t, totalSeenBlocks, seenBlocksBeforePause)

// 	seenBlocksBeforeStop := totalSeenBlocks

// 	// stop
// 	assert.NoError(t, sr.Stop())

// 	time.Sleep(1 * time.Second)

// 	assert.Equal(t, seenBlocksBeforeStop, totalSeenBlocks)
// }

func TestStreamPauseResumeStop(t *testing.T) {
	// hive blocks
	mockHiveBlocks := &test_utils.MockHiveBlockDb{}

	test_utils.RunPlugin(t, mockHiveBlocks)

	var totalBlocks atomic.Int64

	filter := func(op hivego.Operation, blockParams *streamer.BlockParams) bool {
		// count total blocks in filter because this is also
		// called just once like the process function so we can
		// use it to guage if the streamer is still processing
		totalBlocks.Add(1)
		return true
	}

	s := streamer.NewStreamer(&MockBlockClient{}, mockHiveBlocks, []streamer.FilterFunc{filter}, nil, nil)
	assert.NoError(t, s.Init())
	go func() {
		// The streamer rejects its Start promise when stopped — the
		// expected exit, not an error.
		_, _ = s.Start().Await(context.Background())
	}()

	time.Sleep(3 * time.Second)

	totalBlocksBeforePause := totalBlocks.Load()

	s.Pause()

	time.Sleep(1 * time.Second)

	assert.Equal(t, totalBlocksBeforePause, totalBlocks.Load())

	// resume
	s.Resume()

	time.Sleep(1 * time.Second)

	assert.Greater(t, totalBlocks.Load(), totalBlocksBeforePause)

	totalBlocksBeforeStop := totalBlocks.Load()

	// stop
	assert.NoError(t, s.Stop())

	time.Sleep(1 * time.Second)

	assert.Equal(t, totalBlocksBeforeStop, totalBlocks.Load())
}

func TestRestartingProcessingAfterHavingStoppedWithSomeLeft(t *testing.T) {
	// hive blocks
	mockHiveBlocks := &test_utils.MockHiveBlockDb{}

	test_utils.RunPlugin(t, mockHiveBlocks)

	// seed
	seedBlockData(t, mockHiveBlocks, 10)

	processedUpTo, err := mockHiveBlocks.GetLastProcessedBlock()
	assert.NoError(t, err)

	assert.Equal(t, processedUpTo, uint64(0))

	sr := streamer.NewStreamReader(mockHiveBlocks, func(block hive_blocks.HiveBlock, headHeigt *uint64) {}, nil, 0)

	test_utils.RunPlugin(t, sr)

	time.Sleep(3 * time.Second)

	processedUpToAfterStart, err := mockHiveBlocks.GetLastProcessedBlock()
	assert.NoError(t, err)

	assert.Greater(t, processedUpToAfterStart, processedUpTo)

	// now seed up to 20 (10 new)
	seedBlockData(t, mockHiveBlocks, 20)

	time.Sleep(3 * time.Second)

	processedAfterNewSeed, err := mockHiveBlocks.GetLastProcessedBlock()
	assert.NoError(t, err)

	assert.Greater(t, processedAfterNewSeed, processedUpToAfterStart)
}

func TestFilterOrdering(t *testing.T) {
	// hive blocks
	mockHiveBlocks := &test_utils.MockHiveBlockDb{}

	var filter1SeenBlocks atomic.Int64
	var filter2SeenBlocks atomic.Int64

	filter1 := func(op hivego.Operation, blockParams *streamer.BlockParams) bool {
		filter1SeenBlocks.Add(1)
		return true
	}

	filter2 := func(op hivego.Operation, blockParams *streamer.BlockParams) bool {
		filter2SeenBlocks.Add(1)
		return false
	}

	s := streamer.NewStreamer(&MockBlockClient{}, mockHiveBlocks, []streamer.FilterFunc{filter1, filter2}, nil, nil)

	agg := aggregate.New([]aggregate.Plugin{
		mockHiveBlocks,
		s,
	})

	test_utils.RunPlugin(t, agg)

	time.Sleep(3 * time.Second)

	assert.Greater(t, filter1SeenBlocks.Load(), filter2SeenBlocks.Load())
	assert.Equal(t, int64(0), filter2SeenBlocks.Load())
	assert.Greater(t, filter1SeenBlocks.Load(), int64(0))
}

func TestBlockLag(t *testing.T) {

	// hive blocks
	mockHiveBlocks := &test_utils.MockHiveBlockDb{}

	// these are shared package globals — restore them when this test ends
	// so later tests see the init()-established values. The wait lets the
	// streamer goroutines (stopped by the RunPlugin cleanups above, which
	// run before this one) observe their stop and exit before the values
	// they read are restored.
	prevAcceptableBlockLag := streamer.AcceptableBlockLag
	prevDefaultBlockStart := streamer.DefaultBlockStart
	t.Cleanup(func() {
		time.Sleep(500 * time.Millisecond)
		streamer.AcceptableBlockLag = prevAcceptableBlockLag
		streamer.DefaultBlockStart = prevDefaultBlockStart
	})

	streamer.AcceptableBlockLag = 5
	streamer.DefaultBlockStart = dummyBlockHead - 3

	var totalBlocks atomic.Int64

	filter := func(op hivego.Operation, blockParams *streamer.BlockParams) bool {
		totalBlocks.Add(1)
		// allow anything through
		return true
	}

	s := streamer.NewStreamer(&MockBlockClient{}, mockHiveBlocks, []streamer.FilterFunc{filter}, nil, nil)

	agg := aggregate.New([]aggregate.Plugin{
		mockHiveBlocks,
		s,
	})

	test_utils.RunPlugin(t, agg)

	time.Sleep(3 * time.Second)

	// we shoudn't see any blocks because we're
	// only 3 blocks behind which is within the lag
	assert.Equal(t, int64(0), totalBlocks.Load())
	assert.NoError(t, s.Stop())

	// now we should see blocks
	streamer.DefaultBlockStart = dummyBlockHead - 6

	// create new streamer
	s = streamer.NewStreamer(&MockBlockClient{}, mockHiveBlocks, []streamer.FilterFunc{filter}, nil, nil)

	test_utils.RunPlugin(t, s)

	time.Sleep(3 * time.Second)

	assert.Greater(t, totalBlocks.Load(), int64(0))
}

func TestClearingStoredBlocks(t *testing.T) {
	// hive blocks
	mockHiveBlocks := &test_utils.MockHiveBlockDb{}

	test_utils.RunPlugin(t, mockHiveBlocks)

	// seed
	seedBlockData(t, mockHiveBlocks, 10)

	var totalBlocks atomic.Int64

	filter := func(op hivego.Operation, blockParams *streamer.BlockParams) bool {
		totalBlocks.Add(1)
		// allow anything through
		return true
	}

	s := streamer.NewStreamer(&MockBlockClient{}, mockHiveBlocks, []streamer.FilterFunc{filter}, nil, nil)

	test_utils.RunPlugin(t, s)

	time.Sleep(3 * time.Second)

	assert.Greater(t, totalBlocks.Load(), int64(0))

	assert.NotEqual(t, s.StartBlock(), streamer.DefaultBlockStart)

	assert.NoError(t, s.Stop())

	// clear
	assert.NoError(t, mockHiveBlocks.ClearBlocks())

	// restart
	s = streamer.NewStreamer(&MockBlockClient{}, mockHiveBlocks, []streamer.FilterFunc{filter}, nil, nil)
	assert.NoError(t, s.Init()) // just re-init to get start block
	assert.Equal(t, streamer.DefaultBlockStart, s.StartBlock())
}

func TestClearingLastProcessedBlock(t *testing.T) {
	mockHiveBlocks := &test_utils.MockHiveBlockDb{}

	test_utils.RunPlugin(t, mockHiveBlocks)

	seedBlockData(t, mockHiveBlocks, 10)

	totalBlocks := 0

	sr := streamer.NewStreamReader(mockHiveBlocks, func(block hive_blocks.HiveBlock, headHeigt *uint64) {
		totalBlocks++
	}, nil, 0)

	test_utils.RunPlugin(t, sr)

	time.Sleep(3 * time.Second)

	lastProcessedBlock, err := mockHiveBlocks.GetLastProcessedBlock()
	assert.NoError(t, err)

	assert.Greater(t, lastProcessedBlock, uint64(0))

	assert.NoError(t, mockHiveBlocks.StoreLastProcessedBlock(33))

	lastProcessedBlockAfterClear, err := mockHiveBlocks.GetLastProcessedBlock()
	assert.NoError(t, err)

	assert.Equal(t, uint64(33), lastProcessedBlockAfterClear)
}

func TestHeadBlock(t *testing.T) {

	// hive blocks
	mockHiveBlocks := &test_utils.MockHiveBlockDb{}

	test_utils.RunPlugin(t, mockHiveBlocks)

	mockClient := &MockBlockClient{}

	dynData, err := mockClient.GetDynamicGlobalProps()
	assert.NoError(t, err)

	var headBlock map[string]interface{}
	err = json.Unmarshal(dynData, &headBlock)
	assert.NoError(t, err)

	headBlockNum := uint64(headBlock["head_block_number"].(float64))

	s := streamer.NewStreamer(mockClient, mockHiveBlocks, nil, nil, nil)

	test_utils.RunPlugin(t, s)

	time.Sleep(3 * time.Second)

	actual := s.HeadHeight()
	assert.GreaterOrEqual(t, actual, headBlockNum, "head height should be at least the dummy block head")
	assert.LessOrEqual(t, actual, headBlockNum+1, "head height may be head+1 due to 3s ticker increment in trackHeadHeight")
}

func TestDbStoredBlockIntegrity(t *testing.T) {

	// hive blocks
	mockHiveBlocks := &test_utils.MockHiveBlockDb{}

	test_utils.RunPlugin(t, mockHiveBlocks)

	seedBlockData(t, mockHiveBlocks, 10)

	// ensure all blocks are there
	storedBlocks, err := mockHiveBlocks.FetchStoredBlocks(1, 10)
	assert.NoError(t, err)
	assert.Len(t, storedBlocks, 10)

	// ensure all blocks are in order
	for i, block := range storedBlocks {
		assert.Equal(t, uint64(i+1), block.BlockNumber)
	}

	// expected metadata
	expectedTimestamp := "2024-01-01T00:00:00"
	expectedAmount := "1 HIVE"
	expectedType := "custom_json"

	// ensure all blocks have the expected data
	for i, block := range storedBlocks {
		expectedBlockID := fmt.Sprintf("some-block-id-%d", i+1)
		expectedMerkleRoot := fmt.Sprintf("some-merkle-root-%d", i+1)
		expectedTransactionID := fmt.Sprintf("some-tx-id-%d", i+1)

		assert.Equal(t, uint64(i+1), block.BlockNumber)
		assert.Equal(t, expectedBlockID, block.BlockID)
		assert.Equal(t, expectedTimestamp, block.Timestamp)
		assert.Equal(t, expectedMerkleRoot, block.MerkleRoot)

		assert.Len(t, block.Transactions, 1)
		assert.Equal(t, expectedTransactionID, block.Transactions[0].TransactionID)
		assert.Len(t, block.Transactions[0].Operations, 1)
		assert.Equal(t, expectedAmount, block.Transactions[0].Operations[0].Value["amount"])
		assert.Equal(t, expectedType, block.Transactions[0].Operations[0].Type)
	}

}

func TestNestedArrayStructure(t *testing.T) {

	mockHiveBlocks := &test_utils.MockHiveBlockDb{}

	// clear existing data
	assert.NoError(t, mockHiveBlocks.ClearBlocks())

	// a dummy hiveblock that has a nested array structure (e.g. custom_json
	// payloads with arrays of arrays). Blocks are stored and retrieved as
	// plain BSON, so the round trip must preserve the nested arrays.
	originalBlock := hive_blocks.HiveBlock{
		BlockNumber: 123,
		BlockID:     "some-block-id-123",
		Timestamp:   "2024-01-01T00:00:00",
		MerkleRoot:  "123",
		Transactions: []hive_blocks.Tx{
			{
				TransactionID: "some-tx-id-123",
				Operations: []hivego.Operation{
					{
						Value: map[string]interface{}{
							"json": map[string]interface{}{
								"nested_array": []interface{}{
									[]interface{}{"hello", "world"}, // this is our nested array!
									[]interface{}{"foo", "bar"},     // this is our nested array!
								},
							},
						},
						Type: "custom_json_operation",
					},
				},
			},
		},
	}

	// store block
	err := mockHiveBlocks.StoreBlocks(originalBlock.BlockNumber, originalBlock)
	assert.NoError(t, err)

	// fetch stored block directly by its ID (we do this with a 1-wide range)
	fetchedBlocks, err := mockHiveBlocks.FetchStoredBlocks(123, 123)
	assert.NoError(t, err)
	assert.Len(t, fetchedBlocks, 1) // we should only get this 1 block back

	// compare the original and fetched blocks
	//
	// the reason we have to do this is because we retrieve blocks via the
	// collection read path, so we want to ensure the round trip preserves
	// nested array structures
	assert.Equal(t, originalBlock, fetchedBlocks[0])
}
