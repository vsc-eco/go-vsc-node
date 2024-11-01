package streamer_test

// ===== NOTE =====
// despite this streamer relying on live RPC calls, we make the test
// deterministic by mocking the block client service
// ===== NOTE =====

import (
	"context"
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
)

// ===== test utils =====

// cleans up the local test data directory
func setupAndCleanUpDataDir(t *testing.T) {

	t.Cleanup(func() {
		os.RemoveAll("data")
	})

	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-signalChan
		os.RemoveAll("data")
		os.Exit(1)
	}()
}

// ===== mock hive block client service =====

type MockBlockClient struct{}

func (m *MockBlockClient) GetDynamicGlobalProps() ([]byte, error) {
	props, _ := json.Marshal(map[string]interface{}{
		"head_block_number": float64(streamer.DefaultBlockStart * 3), // dummy head, whatever it may be
	})
	return props, nil
}

func (m *MockBlockClient) GetBlockRange(startBlock int, count int) (<-chan hivego.Block, error) {
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

// ===== test cases =====

func TestStreamFiltering(t *testing.T) {
	setupAndCleanUpDataDir(t)
	mockBlockClient := &MockBlockClient{}

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

	os.Chmod("data", 0755)

	// new hive block manager
	hiveBlockDbManager, err := hive_blocks.New(vscDb)
	assert.NoError(t, err)

	totalTxs := 0

	// dummy filters and processing function
	filter := func(op map[string]interface{}) bool {
		totalTxs++

		return op["amount"] != "1 HIVE"
	}

	txsAfterFiltering := 0

	process := func(block hive_blocks.HiveBlock) {
		for _, tx := range block.Transactions {
			txsAfterFiltering += len(tx.Operations)
		}
	}

	// init and start the streamer
	startBlock := 5000
	s := streamer.New(mockBlockClient, hiveBlockDbManager, []streamer.FilterFunc{filter}, process, &startBlock)

	streamer.BlockBatchSize = 10
	streamer.AcceptableBlockLag = 2
	streamer.HeadBlockCheckPollInterval = 100 * time.Millisecond
	streamer.MinTimeBetweenBlockBatchFetches = 100 * time.Millisecond

	assert.NoError(t, s.Init())
	assert.NoError(t, s.Start())
	defer func() { assert.NoError(t, s.Stop()) }()

	// allow time for processing the dummy data
	time.Sleep(3 * time.Second)

	// we've filtered all the txs
	assert.Equal(t, 0, txsAfterFiltering)
	assert.Less(t, txsAfterFiltering, totalTxs)
	assert.NotEqual(t, totalTxs, 0)

}

func TestStreamStatusAndBlockProcessing(t *testing.T) {
	setupAndCleanUpDataDir(t)
	mockBlockClient := &MockBlockClient{}

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

	os.Chmod("data", 0755)

	// new hive block manager
	hiveBlockDbManager, err := hive_blocks.New(vscDb)
	assert.NoError(t, err)

	blocksProcessed := 0

	// dummy filters and processing function
	filter1 := func(op map[string]interface{}) bool { return true }
	filter2 := func(op map[string]interface{}) bool { return true }
	process := func(block hive_blocks.HiveBlock) {
		blocksProcessed++
	}

	// init and start the streamer
	startBlock := 4000
	s := streamer.New(mockBlockClient, hiveBlockDbManager, []streamer.FilterFunc{filter1, filter2}, process, &startBlock)

	streamer.BlockBatchSize = 10
	streamer.AcceptableBlockLag = 2
	streamer.HeadBlockCheckPollInterval = 100 * time.Millisecond
	streamer.MinTimeBetweenBlockBatchFetches = 100 * time.Millisecond

	assert.NoError(t, s.Init())
	assert.NoError(t, s.Start())
	defer func() { assert.NoError(t, s.Stop()) }()

	// allow time for processing the dummy data
	time.Sleep(3 * time.Second)

	// check if we processed all blocks
	assert.GreaterOrEqual(t, blocksProcessed, 10) // we should process at least 1 batch

	blocksProcessedBeforePause := blocksProcessed

	assert.False(t, s.IsPaused())
	assert.False(t, s.IsStopped())

	// pause the streamer
	s.Pause()

	assert.True(t, s.IsPaused())
	assert.False(t, s.IsStopped())

	// resume the streamer
	s.Resume()

	assert.False(t, s.IsPaused())
	assert.False(t, s.IsStopped())

	// allow time for the streamer to resume
	time.Sleep(3 * time.Second)

	// check if we processed more blocks
	assert.Greater(t, blocksProcessed, blocksProcessedBeforePause)

	// stop the streamer
	assert.NoError(t, s.Stop())

	// check if the streamer stopped
	assert.True(t, s.IsStopped())

	// check if given all the time provided (more than streamer.MinTimeBetweenBlockBatchFetches multiple times over)
	// that since `startBlock` is way less than our head, we have at least 3x batches provided, or 30 blocks
	assert.GreaterOrEqual(t, blocksProcessed, 30)
}

func TestFilterOrdering(t *testing.T) {
	setupAndCleanUpDataDir(t)
	mockBlockClient := &MockBlockClient{}

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

	os.Chmod("data", 0755)

	// new hive block manager
	hiveBlockDbManager, err := hive_blocks.New(vscDb)
	assert.NoError(t, err)

	filter1Called := 0
	filter2Called := 0

	// dummy filters and processing function
	filter1 := func(op map[string]interface{}) bool {
		filter1Called++
		return op["amount"] != "1 HIVE"
	}

	filter2 := func(op map[string]interface{}) bool {
		filter2Called++
		return op["amount"] != "1 HIVE"
	}

	process := func(block hive_blocks.HiveBlock) {}

	// init and start the streamer
	startBlock := 5000
	s := streamer.New(mockBlockClient, hiveBlockDbManager, []streamer.FilterFunc{filter1, filter2}, process, &startBlock)

	streamer.BlockBatchSize = 10
	streamer.AcceptableBlockLag = 2
	streamer.HeadBlockCheckPollInterval = 100 * time.Millisecond
	streamer.MinTimeBetweenBlockBatchFetches = 100 * time.Millisecond

	assert.NoError(t, s.Init())
	assert.NoError(t, s.Start())
	defer func() { assert.NoError(t, s.Stop()) }()

	// allow time for processing the dummy data
	time.Sleep(3 * time.Second)

	// check if the filters were called in correct order
	//
	// since we're filtering out all the operations, filter 2 should not be called
	assert.Greater(t, filter1Called, filter2Called)
	assert.Equal(t, filter2Called, 0)
	assert.NotEqual(t, filter1Called, 0)
}

func TestBlockLag(t *testing.T) {
	setupAndCleanUpDataDir(t)
	mockBlockClient := &MockBlockClient{}

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

	os.Chmod("data", 0755)

	// new hive block manager
	hiveBlockDbManager, err := hive_blocks.New(vscDb)
	assert.NoError(t, err)

	// dummy filters and processing function
	filter := func(op map[string]interface{}) bool {
		return true
	}

	process := func(block hive_blocks.HiveBlock) {}

	// init and start the streamer
	startBlock := streamer.DefaultBlockStart*3 - 7 // 7 blocks behind head from our mock head
	s := streamer.New(mockBlockClient, hiveBlockDbManager, []streamer.FilterFunc{filter}, process, &startBlock)

	streamer.BlockBatchSize = 10
	streamer.AcceptableBlockLag = 8 // we tolerate 8 blocks behind head
	streamer.HeadBlockCheckPollInterval = 100 * time.Millisecond
	streamer.MinTimeBetweenBlockBatchFetches = 100 * time.Millisecond

	assert.NoError(t, s.Init())
	assert.NoError(t, s.Start())
	defer func() { assert.NoError(t, s.Stop()) }()

	// allow time for processing the dummy data
	time.Sleep(3 * time.Second)

	// check if we're within the block lag
	assert.Equal(t, s.StartBlock(), startBlock)
}

func TestFindingSettingClearing(t *testing.T) {
	setupAndCleanUpDataDir(t)
	mockBlockClient := &MockBlockClient{}

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

	os.Chmod("data", 0755)

	// new hive block manager
	hiveBlockDbManager, err := hive_blocks.New(vscDb)
	assert.NoError(t, err)
	assert.NoError(t, hiveBlockDbManager.ClearBlocks(context.Background()))

	// dummy filters and processing function
	filter := func(op map[string]interface{}) bool {
		return true
	}

	process := func(block hive_blocks.HiveBlock) {}

	// init and start the streamer
	s := streamer.New(mockBlockClient, hiveBlockDbManager, []streamer.FilterFunc{filter}, process, nil)

	streamer.BlockBatchSize = 10
	streamer.AcceptableBlockLag = 2
	streamer.HeadBlockCheckPollInterval = 100 * time.Millisecond
	streamer.MinTimeBetweenBlockBatchFetches = 100 * time.Millisecond

	assert.NoError(t, s.Init())
	assert.NoError(t, s.Start())
	defer func() { assert.NoError(t, s.Stop()) }()

	assert.Equal(t, s.StartBlock(), streamer.DefaultBlockStart)

	// now we run for a bit to increase this
	time.Sleep(3 * time.Second)

	// now let's stop it
	assert.NoError(t, s.Stop())

	// then let's redefine a new one
	s = streamer.New(mockBlockClient, hiveBlockDbManager, []streamer.FilterFunc{filter}, process, nil)

	// we should no have it start NOT at default since it has SOME amount of blocks its processed
	assert.NoError(t, s.Init())
	assert.NoError(t, s.Start())

	assert.Greater(t, s.StartBlock(), streamer.DefaultBlockStart)

	// pause
	s.Pause()

	time.Sleep(6 * time.Second)

	// now let's test our clear functionality
	assert.NoError(t, hiveBlockDbManager.ClearBlocks(context.Background()))

	assert.NoError(t, s.Stop())

	// now if we redeclare and start, it should be back at default
	s = streamer.New(mockBlockClient, hiveBlockDbManager, []streamer.FilterFunc{filter}, process, nil)

	assert.NoError(t, s.Init())
	assert.Equal(t, streamer.DefaultBlockStart, s.StartBlock())
	assert.NoError(t, s.Start())
}

func TestStartAndHeadBlock(t *testing.T) {
	setupAndCleanUpDataDir(t)
	mockBlockClient := &MockBlockClient{}

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

	os.Chmod("data", 0755)

	// new hive block manager
	hiveBlockDbManager, err := hive_blocks.New(vscDb)
	assert.NoError(t, err)

	// dummy filters and processing function
	filter := func(op map[string]interface{}) bool {
		return true
	}

	process := func(block hive_blocks.HiveBlock) {}

	// init and start the streamer
	s := streamer.New(mockBlockClient, hiveBlockDbManager, []streamer.FilterFunc{filter}, process, nil)

	streamer.BlockBatchSize = 10
	streamer.AcceptableBlockLag = 2
	streamer.HeadBlockCheckPollInterval = 100 * time.Millisecond
	streamer.MinTimeBetweenBlockBatchFetches = 100 * time.Millisecond

	assert.NoError(t, s.Init())
	assert.NoError(t, s.Start())
	defer func() { assert.NoError(t, s.Stop()) }()

	// allow time for processing the dummy data
	time.Sleep(3 * time.Second)

	// check if we're within the block lag
	assert.Greater(t, s.StartBlock(), 1)
	assert.Equal(t, s.HeadHeight(), streamer.DefaultBlockStart*3)
}

func TestFetchStoredBlocks(t *testing.T) {
	setupAndCleanUpDataDir(t)
	mockBlockClient := &MockBlockClient{}

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

	os.Chmod("data", 0755)

	// new hive block manager
	hiveBlockDbManager, err := hive_blocks.New(vscDb)
	assert.NoError(t, err)

	// dummy filters and processing function
	filter := func(op map[string]interface{}) bool {
		return true
	}

	process := func(block hive_blocks.HiveBlock) {}

	// init and start the streamer
	s := streamer.New(mockBlockClient, hiveBlockDbManager, []streamer.FilterFunc{filter}, process, nil)

	streamer.BlockBatchSize = 10
	streamer.AcceptableBlockLag = 2
	streamer.HeadBlockCheckPollInterval = 100 * time.Millisecond
	streamer.MinTimeBetweenBlockBatchFetches = 100 * time.Millisecond

	assert.NoError(t, s.Init())
	assert.NoError(t, s.Start())
	defer func() { assert.NoError(t, s.Stop()) }()

	// allow time for processing the dummy data
	time.Sleep(3 * time.Second)

	// now let's fetch the stored blocks
	blocks, err := hiveBlockDbManager.FetchStoredBlocks(context.Background(), streamer.DefaultBlockStart, streamer.DefaultBlockStart+9)
	assert.NoError(t, err)
	assert.Len(t, blocks, 10)

	// confirm that these blocks match our mock data and are in order
	//
	// also, checks validity of blocks
	for i, block := range blocks {

		// block metadata
		assert.Equal(t, block.BlockNumber, streamer.DefaultBlockStart+i)
		assert.Equal(t, block.BlockID, fmt.Sprintf("fake-block-id-%d", streamer.DefaultBlockStart+i))

		// block txs
		assert.Len(t, block.Transactions, 1)
		assert.Len(t, block.Transactions[0].Operations, 1)
	}
}

func TestProcessAfterFiltering(t *testing.T) {
	setupAndCleanUpDataDir(t)
	mockBlockClient := &MockBlockClient{}

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

	os.Chmod("data", 0755)

	// new hive block manager
	hiveBlockDbManager, err := hive_blocks.New(vscDb)
	assert.NoError(t, err)

	// dummy filters and processing function
	filter := func(op map[string]interface{}) bool {
		return false // don't let any ops through
	}

	processCalled := 0

	process := func(block hive_blocks.HiveBlock) {
		processCalled++
		// we should have no txs since we filtered them all out before this
		assert.Len(t, block.Transactions, 0)
	}

	// init and start the streamer
	s := streamer.New(mockBlockClient, hiveBlockDbManager, []streamer.FilterFunc{filter}, process, nil)

	streamer.BlockBatchSize = 10
	streamer.AcceptableBlockLag = 2
	streamer.HeadBlockCheckPollInterval = 100 * time.Millisecond
	streamer.MinTimeBetweenBlockBatchFetches = 100 * time.Millisecond

	assert.NoError(t, s.Init())
	assert.NoError(t, s.Start())
	defer func() { assert.NoError(t, s.Stop()) }()

	// allow time for processing the dummy data
	time.Sleep(3 * time.Second)

	// check if we processed all blocks
	assert.GreaterOrEqual(t, processCalled, 10) // we should process at least 1 batch
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
	assert.NoError(t, hiveBlockDbManager.ClearBlocks(context.Background()))

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
				Operations: []map[string]interface{}{
					{
						"json": map[string]interface{}{
							"nested_array": []interface{}{
								[]interface{}{"hello", "world"}, // this is our nested array!
								[]interface{}{"foo", "bar"},     // this is our nested array!
							},
						},
					},
				},
			},
		},
	}

	// store block
	err = hiveBlockDbManager.StoreBlock(context.Background(), originalBlock)
	assert.NoError(t, err)

	// fetch stored block directly by its ID (we do this with a 1-wide range)
	fetchedBlocks, err := hiveBlockDbManager.FetchStoredBlocks(context.Background(), 123, 123)
	assert.NoError(t, err)
	assert.Len(t, fetchedBlocks, 1) // we should only get this 1 block back

	// compare the original and fetched blocks
	//
	// the reason we have to do this is because we store it internally in a different format so we
	// want to ensure that our retrieval and conversion function is working correctly
	assert.Equal(t, originalBlock, &fetchedBlocks[0])
}

// todo: vault's experiment
func TestVaultecExperiments(t *testing.T) {
	return // todo: remove for further work
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

	os.Chmod("data", 0755)

	// new hive block manager
	hiveBlockDbManager, err := hive_blocks.New(vscDb)
	assert.NoError(t, err)

	// dummy filters and processing function
	filter := func(op map[string]interface{}) bool {
		return true
	}

	process := func(block hive_blocks.HiveBlock) {
		fmt.Println("PROCESSING BLOCK:", block.BlockNumber, "WITH TXS:", len(block.Transactions))

		// for _, tx := range block.Transactions {
		// 	fmt.Println(tx)
		// 	for _,op := range tx.Operations {

		// 	}

		// }
	}

	client := hivego.NewHiveRpc("https://api.hive.blog") // https://hive-api.web3telekom.xyz

	streamer.BlockBatchSize = 100
	// streamer.AcceptableBlockLag = 2
	// streamer.HeadBlockCheckPollInterval = 3 * time.Second
	streamer.MinTimeBetweenBlockBatchFetches = 3 * time.Second

	// init and start the streamer
	s := streamer.New(client, hiveBlockDbManager, []streamer.FilterFunc{filter}, process, &[]int{81614765}[0])

	assert.NoError(t, s.Init())
	assert.NoError(t, s.Start())
	defer func() { assert.NoError(t, s.Stop()) }()

	select {}
}
