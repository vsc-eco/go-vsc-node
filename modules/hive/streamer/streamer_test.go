package streamer_test

// ===== NOTE =====
// despite this streamer relying on live RPC calls, we make the test
// deterministic by mocking the block client service
// ===== NOTE =====

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"
	"vsc-node/modules/db/vsc/hive_blocks"
	"vsc-node/modules/hive/streamer"

	"github.com/stretchr/testify/assert"
	"github.com/vsc-eco/hivego"
	"golang.org/x/exp/rand"
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

// cleans up the local test data directory
func setupAndCleanUpDataDir(t *testing.T) {

	t.Cleanup(func() {
		os.RemoveAll("data")
	})

	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt, syscall.SIGTERM)

	// block until data dir is removed
	for {
		if _, err := os.Stat("data"); os.IsNotExist(err) {
			break
		}
		time.Sleep(time.Millisecond * 100)
	}

	go func() {
		<-signalChan
		os.RemoveAll("data")
		os.Exit(1)
	}()
}

func seedBlockData(t *testing.T, hiveBlocks hive_blocks.HiveBlocks, n int) {

	for i := 1; i <= n; i++ {
		// a dummy hiveblock
		block := &hive_blocks.HiveBlock{
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
		err := hiveBlocks.StoreBlock(block)
		assert.NoError(t, err)
	}
}

// ===== mock hive block client service =====

type MockBlockClient struct{}

func (m *MockBlockClient) GetDynamicGlobalProps() ([]byte, error) {
	// await for random duration to simulate real-world scenario
	//
	// to contribute to innate thread randomness since these tests because of that can't be 100% deterministic anyway
	time.Sleep(time.Millisecond * time.Duration(10+rand.Intn(90)))
	props, _ := json.Marshal(map[string]interface{}{
		"head_block_number": float64(dummyBlockHead), // dummy head, whatever it may be
	})
	return props, nil
}

func (m *MockBlockClient) GetBlockRange(startBlock int, count int) (<-chan hivego.Block, error) {
	// await for random duration to simulate real-world scenario
	//
	// to contribute to innate thread randomness since these tests because of that can't be 100% deterministic anyway
	blockChannel := make(chan hivego.Block, count)
	go func() {
		for i := 0; i < count; i++ {
			blockNumber := startBlock + i
			blockChannel <- hivego.Block{
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
		close(blockChannel)
	}()
	return blockChannel, nil
}

// ===== tests =====

func TestFetchStoreBlocks(t *testing.T) {

	// cleanup
	setupAndCleanUpDataDir(t)

	// db
	d := db.New()
	assert.NoError(t, d.Init())
	assert.NoError(t, d.Start())
	defer func() { assert.NoError(t, d.Stop()) }()

	// vsc db
	vscDb := vsc.New(d)
	assert.NoError(t, vscDb.Init())
	assert.NoError(t, vscDb.Start())
	defer func() { assert.NoError(t, vscDb.Stop()) }()

	// perms
	os.Chmod("data", 0755)

	// hive blocks
	hiveBlocks, err := hive_blocks.New(vscDb)
	assert.NoError(t, err)

	s := streamer.NewStreamer(&MockBlockClient{}, hiveBlocks, nil, nil)
	assert.NoError(t, s.Init())
	assert.NoError(t, s.Start())
	defer func() { assert.NoError(t, s.Stop()) }()

	totalBlksReceived := 0

	process := func(block hive_blocks.HiveBlock) {
		totalBlksReceived++
	}

	sr := streamer.NewStreamReader(hiveBlocks, process)
	assert.NoError(t, sr.Init())
	assert.NoError(t, sr.Start())
	defer func() { assert.NoError(t, sr.Stop()) }()

	time.Sleep(3 * time.Second)

	assert.Greater(t, totalBlksReceived, 0)
}

func TestStartBlock(t *testing.T) {

	// cleanup
	setupAndCleanUpDataDir(t)

	// db
	d := db.New()
	assert.NoError(t, d.Init())
	assert.NoError(t, d.Start())
	defer func() { assert.NoError(t, d.Stop()) }()

	// vsc db
	vscDb := vsc.New(d)
	assert.NoError(t, vscDb.Init())
	assert.NoError(t, vscDb.Start())
	defer func() { assert.NoError(t, vscDb.Stop()) }()

	// perms
	os.Chmod("data", 0755)

	// hive blocks
	hiveBlocks, err := hive_blocks.New(vscDb)
	assert.NoError(t, err)

	s := streamer.NewStreamer(&MockBlockClient{}, hiveBlocks, nil, nil)
	assert.NoError(t, s.Init())

	// should default to our default if we don't specify and instead input nil
	assert.Equal(t, streamer.DefaultBlockStart, s.StartBlock())

	assert.NoError(t, s.Start())
	assert.NoError(t, s.Stop())

	s = streamer.NewStreamer(&MockBlockClient{}, hiveBlocks, nil, &[]int{99}[0])
	assert.NoError(t, s.Init())

	// should be the value we input
	assert.Equal(t, 99, s.StartBlock())

	assert.NoError(t, s.Start())
	assert.NoError(t, s.Stop())
}

func TestIntensePolling(t *testing.T) {
	// cleanup
	setupAndCleanUpDataDir(t)

	// db
	d := db.New()
	assert.NoError(t, d.Init())
	assert.NoError(t, d.Start())
	defer func() { assert.NoError(t, d.Stop()) }()

	// vsc db
	vscDb := vsc.New(d)
	assert.NoError(t, vscDb.Init())
	assert.NoError(t, vscDb.Start())
	defer func() { assert.NoError(t, vscDb.Stop()) }()

	// perms
	os.Chmod("data", 0755)

	// hive blocks
	hiveBlocks, err := hive_blocks.New(vscDb)
	assert.NoError(t, err)

	s := streamer.NewStreamer(&MockBlockClient{}, hiveBlocks, nil, nil)
	assert.NoError(t, s.Init())
	assert.NoError(t, s.Start())
	defer func() { assert.NoError(t, s.Stop()) }()

	seenBlocks := make(map[int]int)

	sr := streamer.NewStreamReader(hiveBlocks, func(block hive_blocks.HiveBlock) {
		seenBlocks[block.BlockNumber]++
	})
	assert.NoError(t, sr.Init())
	assert.NoError(t, sr.Start())
	defer func() { assert.NoError(t, sr.Stop()) }()

	time.Sleep(3 * time.Second)

	// ensure in entire map, no dupes!
	for _, v := range seenBlocks {
		assert.Equal(t, 1, v)
	}
}

func TestStreamFilter(t *testing.T) {

	// cleanup
	setupAndCleanUpDataDir(t)

	// db
	d := db.New()
	assert.NoError(t, d.Init())
	assert.NoError(t, d.Start())
	defer func() { assert.NoError(t, d.Stop()) }()

	// vsc db
	vscDb := vsc.New(d)
	assert.NoError(t, vscDb.Init())
	assert.NoError(t, vscDb.Start())
	defer func() { assert.NoError(t, vscDb.Stop()) }()

	// perms
	os.Chmod("data", 0755)

	// hive blocks
	hiveBlocks, err := hive_blocks.New(vscDb)
	assert.NoError(t, err)

	filter := func(op hivego.Operation) bool {
		// filter everything!
		return op.Value["amount"] != "1 HIVE"
	}

	s := streamer.NewStreamer(&MockBlockClient{}, hiveBlocks, []streamer.FilterFunc{filter}, nil)
	assert.NoError(t, s.Init())
	assert.NoError(t, s.Start())
	defer func() { assert.NoError(t, s.Stop()) }()

	txsReceived := 0

	process := func(block hive_blocks.HiveBlock) {
		if len(block.Timestamp) > 0 {
			txsReceived += len(block.Transactions)
		}
	}

	sr := streamer.NewStreamReader(hiveBlocks, process)
	assert.NoError(t, sr.Init())
	assert.NoError(t, sr.Start())
	defer func() { assert.NoError(t, sr.Stop()) }()

	time.Sleep(3 * time.Second)

	assert.Equal(t, 0, txsReceived)
}

func TestPersistingBlocksStored(t *testing.T) {
	// cleanup
	setupAndCleanUpDataDir(t)

	// db
	d := db.New()
	assert.NoError(t, d.Init())
	assert.NoError(t, d.Start())
	defer func() { assert.NoError(t, d.Stop()) }()

	// vsc db
	vscDb := vsc.New(d)
	assert.NoError(t, vscDb.Init())
	assert.NoError(t, vscDb.Start())
	defer func() { assert.NoError(t, vscDb.Stop()) }()

	// perms
	os.Chmod("data", 0755)

	// hive blocks
	hiveBlocks, err := hive_blocks.New(vscDb)
	assert.NoError(t, err)

	// start at 0
	s := streamer.NewStreamer(&MockBlockClient{}, hiveBlocks, nil, &[]int{0}[0])
	assert.NoError(t, s.Init())
	assert.NoError(t, s.Start())

	time.Sleep(3 * time.Second)

	assert.NoError(t, s.Stop())

	gotToBlock, err := hiveBlocks.GetHighestBlock()
	assert.NoError(t, err)

	assert.Greater(t, gotToBlock, 0)

	s = streamer.NewStreamer(&MockBlockClient{}, hiveBlocks, nil, nil)
	assert.NoError(t, s.Init())
	assert.NoError(t, s.Start())
	defer func() { assert.NoError(t, s.Stop()) }()

	time.Sleep(3 * time.Second)

	gotToBlockTry2, err := hiveBlocks.GetHighestBlock()
	assert.NoError(t, err)

	assert.Greater(t, gotToBlockTry2, streamer.DefaultBlockStart)
	assert.Greater(t, gotToBlockTry2, gotToBlock)
}

func TestPersistingBlocksProcessed(t *testing.T) {
	// cleanup
	setupAndCleanUpDataDir(t)

	// db
	d := db.New()
	assert.NoError(t, d.Init())
	assert.NoError(t, d.Start())
	defer func() { assert.NoError(t, d.Stop()) }()

	// vsc db
	vscDb := vsc.New(d)
	assert.NoError(t, vscDb.Init())
	assert.NoError(t, vscDb.Start())
	defer func() { assert.NoError(t, vscDb.Stop()) }()

	// perms
	os.Chmod("data", 0755)

	// hive blocks
	hiveBlocks, err := hive_blocks.New(vscDb)
	assert.NoError(t, err)

	// start at default
	s := streamer.NewStreamer(&MockBlockClient{}, hiveBlocks, nil, nil)
	assert.NoError(t, s.Init())
	assert.NoError(t, s.Start())
	defer func() { assert.NoError(t, s.Stop()) }()

	lastProcessedBlk := -1

	sr := streamer.NewStreamReader(hiveBlocks, func(block hive_blocks.HiveBlock) {
		lastProcessedBlk = block.BlockNumber
	})
	assert.NoError(t, sr.Init())
	assert.NoError(t, sr.Start())

	time.Sleep(2 * time.Second)

	assert.Greater(t, lastProcessedBlk, streamer.DefaultBlockStart)

	s.Pause()
	assert.NoError(t, sr.Stop())

	newLastProcessedBlk, err := hiveBlocks.GetLastProcessedBlock()
	assert.NoError(t, err)

	assert.Greater(t, newLastProcessedBlk, streamer.DefaultBlockStart)
	assert.Equal(t, lastProcessedBlk, newLastProcessedBlk)

	assert.NoError(t, sr.Stop())

	resumedLastProcessedBlk := -1

	s.Resume()

	// redefine stream reader and see if it picks up where it left off
	sr = streamer.NewStreamReader(hiveBlocks, func(block hive_blocks.HiveBlock) {
		resumedLastProcessedBlk = block.BlockNumber
	})
	assert.NoError(t, sr.Init())
	assert.NoError(t, sr.Start())
	defer func() { assert.NoError(t, sr.Stop()) }()

	time.Sleep(2 * time.Second)

	assert.Greater(t, resumedLastProcessedBlk, streamer.DefaultBlockStart)
	assert.Greater(t, resumedLastProcessedBlk, newLastProcessedBlk)
}

func TestBlockProcessing(t *testing.T) {
	// cleanup
	setupAndCleanUpDataDir(t)

	// db
	d := db.New()
	assert.NoError(t, d.Init())
	assert.NoError(t, d.Start())
	defer func() { assert.NoError(t, d.Stop()) }()

	// vsc db
	vscDb := vsc.New(d)
	assert.NoError(t, vscDb.Init())
	assert.NoError(t, vscDb.Start())
	defer func() { assert.NoError(t, vscDb.Stop()) }()

	// perms
	os.Chmod("data", 0755)

	// hive blocks
	hiveBlocks, err := hive_blocks.New(vscDb)
	assert.NoError(t, err)

	// seed
	seedBlockData(t, hiveBlocks, 10)

	totalSeenBlocks := 0

	sr := streamer.NewStreamReader(hiveBlocks, func(block hive_blocks.HiveBlock) {
		totalSeenBlocks++
	})
	assert.NoError(t, sr.Init())
	assert.NoError(t, sr.Start())
	defer func() { assert.NoError(t, sr.Stop()) }()

	time.Sleep(3 * time.Second)

	assert.Equal(t, 10, totalSeenBlocks)
}

func TestStreamReaderPauseResumeStop(t *testing.T) {
	// cleanup
	setupAndCleanUpDataDir(t)

	// db
	d := db.New()
	assert.NoError(t, d.Init())
	assert.NoError(t, d.Start())
	defer func() { assert.NoError(t, d.Stop()) }()

	// vsc db
	vscDb := vsc.New(d)
	assert.NoError(t, vscDb.Init())
	assert.NoError(t, vscDb.Start())
	defer func() { assert.NoError(t, vscDb.Stop()) }()

	// perms
	os.Chmod("data", 0755)

	// hive blocks
	hiveBlocks, err := hive_blocks.New(vscDb)
	assert.NoError(t, err)

	// seed
	seedBlockData(t, hiveBlocks, 10)

	totalSeenBlocks := 0

	sr := streamer.NewStreamReader(hiveBlocks, func(block hive_blocks.HiveBlock) {
		totalSeenBlocks++
	})
	assert.NoError(t, sr.Init())
	assert.NoError(t, sr.Start())
	defer func() { assert.NoError(t, sr.Stop()) }()

	time.Sleep(3 * time.Second)

	seenBlocksBeforePause := totalSeenBlocks

	sr.Pause()

	time.Sleep(1 * time.Second)

	assert.Equal(t, seenBlocksBeforePause, totalSeenBlocks)

	// resume
	sr.Resume()

	// seed
	seedBlockData(t, hiveBlocks, 20)

	time.Sleep(1 * time.Second)

	assert.Greater(t, totalSeenBlocks, seenBlocksBeforePause)

	seenBlocksBeforeStop := totalSeenBlocks

	// stop
	sr.Stop()

	time.Sleep(1 * time.Second)

	assert.Equal(t, seenBlocksBeforeStop, totalSeenBlocks)
}

func TestStreamPauseResumeStop(t *testing.T) {
	// cleanup
	setupAndCleanUpDataDir(t)

	// db
	d := db.New()
	assert.NoError(t, d.Init())
	assert.NoError(t, d.Start())
	defer func() { assert.NoError(t, d.Stop()) }()

	// vsc db
	vscDb := vsc.New(d)
	assert.NoError(t, vscDb.Init())
	assert.NoError(t, vscDb.Start())
	defer func() { assert.NoError(t, vscDb.Stop()) }()

	// perms
	os.Chmod("data", 0755)

	// hive blocks
	hiveBlocks, err := hive_blocks.New(vscDb)
	assert.NoError(t, err)

	totalBlocks := 0

	filter := func(op hivego.Operation) bool {
		// count total blocks in filter because this is also
		// called just once like the process function so we can
		// use it to guage if the streamer is still processing
		totalBlocks++
		return true
	}

	s := streamer.NewStreamer(&MockBlockClient{}, hiveBlocks, []streamer.FilterFunc{filter}, nil)
	assert.NoError(t, s.Init())
	assert.NoError(t, s.Start())
	defer func() { assert.NoError(t, s.Stop()) }()

	time.Sleep(3 * time.Second)

	totalBlocksBeforePause := totalBlocks

	s.Pause()

	time.Sleep(1 * time.Second)

	assert.Equal(t, totalBlocksBeforePause, totalBlocks)

	// resume
	s.Resume()

	time.Sleep(1 * time.Second)

	assert.Greater(t, totalBlocks, totalBlocksBeforePause)

	totalBlocksBeforeStop := totalBlocks

	// stop
	s.Stop()

	time.Sleep(1 * time.Second)

	assert.Equal(t, totalBlocksBeforeStop, totalBlocks)
}

func TestRestartingProcessingAfterHavingStoppedWithSomeLeft(t *testing.T) {
	// cleanup
	setupAndCleanUpDataDir(t)

	// db
	d := db.New()
	assert.NoError(t, d.Init())
	assert.NoError(t, d.Start())
	defer func() { assert.NoError(t, d.Stop()) }()

	// vsc db
	vscDb := vsc.New(d)
	assert.NoError(t, vscDb.Init())
	assert.NoError(t, vscDb.Start())
	defer func() { assert.NoError(t, vscDb.Stop()) }()

	// perms
	os.Chmod("data", 0755)

	// hive blocks
	hiveBlocks, err := hive_blocks.New(vscDb)
	assert.NoError(t, err)

	// seed
	seedBlockData(t, hiveBlocks, 10)

	processedUpTo, err := hiveBlocks.GetLastProcessedBlock()
	assert.NoError(t, err)

	assert.Equal(t, processedUpTo, -1) // -1 means no blocks processed yet

	sr := streamer.NewStreamReader(hiveBlocks, func(block hive_blocks.HiveBlock) {})
	assert.NoError(t, sr.Init())
	assert.NoError(t, sr.Start())
	defer func() { assert.NoError(t, sr.Stop()) }()

	time.Sleep(3 * time.Second)

	processedUpToAfterStart, err := hiveBlocks.GetLastProcessedBlock()
	assert.NoError(t, err)

	assert.Greater(t, processedUpToAfterStart, processedUpTo)

	// now seed up to 20 (10 new)
	seedBlockData(t, hiveBlocks, 20)

	time.Sleep(3 * time.Second)

	processedAfterNewSeed, err := hiveBlocks.GetLastProcessedBlock()
	assert.NoError(t, err)

	assert.Greater(t, processedAfterNewSeed, processedUpToAfterStart)
}

func TestFilterOrdering(t *testing.T) {
	// cleanup
	setupAndCleanUpDataDir(t)

	// db
	d := db.New()
	assert.NoError(t, d.Init())
	assert.NoError(t, d.Start())
	defer func() { assert.NoError(t, d.Stop()) }()

	// vsc db
	vscDb := vsc.New(d)
	assert.NoError(t, vscDb.Init())
	assert.NoError(t, vscDb.Start())
	defer func() { assert.NoError(t, vscDb.Stop()) }()

	// perms
	os.Chmod("data", 0755)

	// hive blocks
	hiveBlocks, err := hive_blocks.New(vscDb)
	assert.NoError(t, err)

	filter1SeenBlocks := 0
	filter2SeenBlocks := 0

	filter1 := func(op hivego.Operation) bool {
		// filter everything
		filter1SeenBlocks++
		return false
	}

	filter2 := func(op hivego.Operation) bool {
		// filter nothing
		filter2SeenBlocks++
		return true
	}

	s := streamer.NewStreamer(&MockBlockClient{}, hiveBlocks, []streamer.FilterFunc{filter1, filter2}, nil)
	assert.NoError(t, s.Init())
	assert.NoError(t, s.Start())
	defer func() { assert.NoError(t, s.Stop()) }()

	time.Sleep(3 * time.Second)

	assert.Greater(t, filter1SeenBlocks, filter2SeenBlocks)
	assert.Equal(t, 0, filter2SeenBlocks)
	assert.Greater(t, filter1SeenBlocks, 0)
}

func TestBlockLag(t *testing.T) {
	// cleanup
	setupAndCleanUpDataDir(t)

	// db
	d := db.New()
	assert.NoError(t, d.Init())
	assert.NoError(t, d.Start())
	defer func() { assert.NoError(t, d.Stop()) }()

	// vsc db
	vscDb := vsc.New(d)
	assert.NoError(t, vscDb.Init())
	assert.NoError(t, vscDb.Start())
	defer func() { assert.NoError(t, vscDb.Stop()) }()

	// perms
	os.Chmod("data", 0755)

	// hive blocks
	hiveBlocks, err := hive_blocks.New(vscDb)
	assert.NoError(t, err)

	streamer.AcceptableBlockLag = 5
	streamer.DefaultBlockStart = dummyBlockHead - 3

	totalBlocks := 0

	filter := func(op hivego.Operation) bool {
		totalBlocks++
		// allow anything through
		return true
	}

	s := streamer.NewStreamer(&MockBlockClient{}, hiveBlocks, []streamer.FilterFunc{filter}, nil)
	assert.NoError(t, s.Init())
	assert.Equal(t, streamer.DefaultBlockStart, s.StartBlock())
	assert.NoError(t, s.Start())

	time.Sleep(3 * time.Second)

	// we shoudn't see any blocks because we're
	// only 3 blocks behind which is within the lag
	assert.Equal(t, 0, totalBlocks)
	assert.NoError(t, s.Stop())

	// now we should see blocks
	streamer.DefaultBlockStart = dummyBlockHead - 6

	// create new streamer
	s = streamer.NewStreamer(&MockBlockClient{}, hiveBlocks, []streamer.FilterFunc{filter}, nil)
	assert.NoError(t, s.Init())
	assert.NoError(t, s.Start())
	defer func() { assert.NoError(t, s.Stop()) }()

	time.Sleep(3 * time.Second)

	assert.Greater(t, totalBlocks, 0)
}

func TestClearingStoredBlocks(t *testing.T) {
	// cleanup
	setupAndCleanUpDataDir(t)

	// db
	d := db.New()
	assert.NoError(t, d.Init())
	assert.NoError(t, d.Start())
	defer func() { assert.NoError(t, d.Stop()) }()

	// vsc db
	vscDb := vsc.New(d)
	assert.NoError(t, vscDb.Init())
	assert.NoError(t, vscDb.Start())
	defer func() { assert.NoError(t, vscDb.Stop()) }()

	// perms
	os.Chmod("data", 0755)

	// hive blocks
	hiveBlocks, err := hive_blocks.New(vscDb)
	assert.NoError(t, err)

	// seed
	seedBlockData(t, hiveBlocks, 10)

	totalBlocks := 0

	filter := func(op hivego.Operation) bool {
		totalBlocks++
		// allow anything through
		return true
	}

	s := streamer.NewStreamer(&MockBlockClient{}, hiveBlocks, []streamer.FilterFunc{filter}, nil)
	assert.NoError(t, s.Init())
	assert.NoError(t, s.Start())

	time.Sleep(3 * time.Second)

	assert.Greater(t, totalBlocks, 0)

	assert.NotEqual(t, s.StartBlock(), streamer.DefaultBlockStart)

	assert.NoError(t, s.Stop())

	// clear
	assert.NoError(t, hiveBlocks.ClearBlocks())

	// restart
	s = streamer.NewStreamer(&MockBlockClient{}, hiveBlocks, []streamer.FilterFunc{filter}, nil)
	assert.NoError(t, s.Init())
	assert.Equal(t, streamer.DefaultBlockStart, s.StartBlock())
	assert.NoError(t, s.Start())
	assert.NoError(t, s.Stop())
}

func TestClearingLastProcessedBlock(t *testing.T) {
	// cleanup
	setupAndCleanUpDataDir(t)

	// db
	d := db.New()
	assert.NoError(t, d.Init())
	assert.NoError(t, d.Start())
	defer func() { assert.NoError(t, d.Stop()) }()

	// vsc db
	vscDb := vsc.New(d)
	assert.NoError(t, vscDb.Init())
	assert.NoError(t, vscDb.Start())
	defer func() { assert.NoError(t, vscDb.Stop()) }()

	// perms
	os.Chmod("data", 0755)

	// hive blocks
	hiveBlocks, err := hive_blocks.New(vscDb)
	assert.NoError(t, err)

	seedBlockData(t, hiveBlocks, 10)

	totalBlocks := 0

	sr := streamer.NewStreamReader(hiveBlocks, func(block hive_blocks.HiveBlock) {
		totalBlocks++
	})
	assert.NoError(t, sr.Init())
	assert.NoError(t, sr.Start())
	defer func() { assert.NoError(t, sr.Stop()) }()

	time.Sleep(3 * time.Second)

	lastProcessedBlock, err := hiveBlocks.GetLastProcessedBlock()
	assert.NoError(t, err)

	assert.Greater(t, lastProcessedBlock, 0)

	assert.NoError(t, hiveBlocks.StoreLastProcessedBlock(33))

	lastProcessedBlockAfterClear, err := hiveBlocks.GetLastProcessedBlock()
	assert.NoError(t, err)

	assert.Equal(t, 33, lastProcessedBlockAfterClear)
}

func TestHeadBlock(t *testing.T) {
	// cleanup
	setupAndCleanUpDataDir(t)

	// db
	d := db.New()
	assert.NoError(t, d.Init())
	assert.NoError(t, d.Start())
	defer func() { assert.NoError(t, d.Stop()) }()

	// vsc db
	vscDb := vsc.New(d)
	assert.NoError(t, vscDb.Init())
	assert.NoError(t, vscDb.Start())
	defer func() { assert.NoError(t, vscDb.Stop()) }()

	// perms
	os.Chmod("data", 0755)

	// hive blocks
	hiveBlocks, err := hive_blocks.New(vscDb)
	assert.NoError(t, err)

	mockClient := &MockBlockClient{}

	dynData, err := mockClient.GetDynamicGlobalProps()
	assert.NoError(t, err)

	var headBlock map[string]interface{}
	err = json.Unmarshal(dynData, &headBlock)
	assert.NoError(t, err)

	headBlockNum := int(headBlock["head_block_number"].(float64))

	s := streamer.NewStreamer(mockClient, hiveBlocks, nil, nil)
	assert.NoError(t, s.Init())
	assert.NoError(t, s.Start())
	defer func() { assert.NoError(t, s.Stop()) }()

	time.Sleep(3 * time.Second)

	assert.Equal(t, headBlockNum, s.HeadHeight())
}

func TestDbStoredBlockIntegrity(t *testing.T) {
	// cleanup
	setupAndCleanUpDataDir(t)

	// db
	d := db.New()
	assert.NoError(t, d.Init())
	assert.NoError(t, d.Start())
	defer func() { assert.NoError(t, d.Stop()) }()

	// vsc db
	vscDb := vsc.New(d)
	assert.NoError(t, vscDb.Init())
	assert.NoError(t, vscDb.Start())
	defer func() { assert.NoError(t, vscDb.Stop()) }()

	// perms
	os.Chmod("data", 0755)

	// hive blocks
	hiveBlocks, err := hive_blocks.New(vscDb)
	assert.NoError(t, err)

	seedBlockData(t, hiveBlocks, 10)

	// ensure all blocks are there
	storedBlocks, err := hiveBlocks.FetchStoredBlocks(1, 10)
	assert.NoError(t, err)
	assert.Len(t, storedBlocks, 10)

	// ensure all blocks are in order
	for i, block := range storedBlocks {
		assert.Equal(t, i+1, block.BlockNumber)
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

		assert.Equal(t, i+1, block.BlockNumber)
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
	setupAndCleanUpDataDir(t)

	// init the db
	d := db.New()
	assert.NoError(t, d.Init())
	assert.NoError(t, d.Start())
	defer func() { assert.NoError(t, d.Stop()) }()

	// init the vsc db
	vscDb := vsc.New(d)
	assert.NoError(t, vscDb.Init())
	assert.NoError(t, vscDb.Start())
	defer func() { assert.NoError(t, vscDb.Stop()) }()

	os.Chmod("data", 0755)

	hiveBlockDbManager, err := hive_blocks.New(vscDb)
	assert.NoError(t, err)

	// clear existing data
	assert.NoError(t, hiveBlockDbManager.ClearBlocks())

	// a dummy hiveblock that has a nested array structure that
	// should fail when stored in mongoDB norally, BUT, with our
	// conversion function, this should now work
	originalBlock := &hive_blocks.HiveBlock{
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
	err = hiveBlockDbManager.StoreBlock(originalBlock)
	assert.NoError(t, err)

	// fetch stored block directly by its ID (we do this with a 1-wide range)
	fetchedBlocks, err := hiveBlockDbManager.FetchStoredBlocks(123, 123)
	assert.NoError(t, err)
	assert.Len(t, fetchedBlocks, 1) // we should only get this 1 block back

	// compare the original and fetched blocks
	//
	// the reason we have to do this is because we store it internally in a different format so we
	// want to ensure that our retrieval and conversion function is working correctly
	assert.Equal(t, originalBlock, &fetchedBlocks[0])
}

// todo: Vaultec's experiments
func TestVaultecExperiments(t *testing.T) {
	return

	// cleanup
	setupAndCleanUpDataDir(t)

	// db
	d := db.New()
	assert.NoError(t, d.Init())
	assert.NoError(t, d.Start())
	defer func() { assert.NoError(t, d.Stop()) }()

	// vsc db
	vscDb := vsc.New(d)
	assert.NoError(t, vscDb.Init())
	assert.NoError(t, vscDb.Start())
	defer func() { assert.NoError(t, vscDb.Stop()) }()

	// perms
	os.Chmod("data", 0755)

	// slow down the streamer a bit for real data
	streamer.AcceptableBlockLag = 0
	streamer.BlockBatchSize = 100
	streamer.DefaultBlockStart = 81614028
	streamer.HeadBlockCheckPollIntervalBeforeFirstUpdate = time.Millisecond * 1500
	streamer.MinTimeBetweenBlockBatchFetches = time.Millisecond * 1500
	streamer.DbPollInterval = time.Millisecond * 500

	hiveBlocks, err := hive_blocks.New(vscDb)
	assert.NoError(t, err)

	filter := func(op hivego.Operation) bool { return true }
	client := hivego.NewHiveRpc("https://api.hive.blog")
	s := streamer.NewStreamer(client, hiveBlocks, []streamer.FilterFunc{filter}, nil)
	assert.NoError(t, s.Init())
	assert.NoError(t, s.Start())
	defer func() { assert.NoError(t, s.Stop()) }()

	process := func(block hive_blocks.HiveBlock) {
		fmt.Printf("block #: %v\n", block.Transactions)
	}
	sr := streamer.NewStreamReader(hiveBlocks, process)
	assert.NoError(t, sr.Init())
	assert.NoError(t, sr.Start())
	defer func() { assert.NoError(t, sr.Stop()) }()

	select {}
}
