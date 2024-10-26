package streamer_test

// import (
// 	"sync"
// 	"testing"
// 	"time"

// 	"vsc-node/modules/db"
// 	"vsc-node/modules/db/vsc"
// 	"vsc-node/modules/db/vsc/hive_blocks"
// 	hiveStreamer "vsc-node/modules/hive/streamer"

// 	"github.com/stretchr/testify/assert"
// )

// // a dummy filter that only accepts transfer ops
// func dummyTransferFiler(op hive_blocks.Op) bool {
// 	return op.Type == "transfer_operation"
// }

// // a dummy filter that only accepts custom_json ops
// func dummyJsonFilter(op hive_blocks.Op) bool {
// 	return op.Type == "custom_json_operation"
// }

// // a dummy block processer
// func dummyBlockProcesser(block hive_blocks.HiveBlock) error {
// 	return nil
// }

// // ensures that the streamer starts from the last processed block
// func TestStreamerStartFromLastProcessedBlock(t *testing.T) {
// 	// inits the db
// 	dbInstance := db.New()
// 	assert.NoError(t, dbInstance.Init())
// 	assert.NoError(t, dbInstance.Start())
// 	defer dbInstance.Stop()

// 	vscDb := vsc.New(dbInstance)
// 	assert.NoError(t, vscDb.Init())
// 	assert.NoError(t, vscDb.Start())
// 	defer vscDb.Stop()

// 	// clears the blocks in case there is any prev state
// 	hiveBlocks := hive_blocks.New(vscDb)
// 	assert.NoError(t, hiveBlocks.ClearBlocks())

// 	// insert a "last processed block" to simulate prev processing
// 	assert.NoError(t, hiveBlocks.StoreLastProcessedBlock(100))

// 	// create the streamer without a specificied specific start block
// 	filters := []hiveStreamer.FilterFunc{dummyTransferFiler, dummyJsonFilter}
// 	streamer := hiveStreamer.New(hiveBlocks, filters, dummyBlockProcesser, nil, nil)

// 	// init the streamer
// 	assert.NoError(t, streamer.Init())

// 	// asert that the startBlock is set to the next block after the last processed one
// 	assert.Equal(t, 101, streamer.GetBlockStartedAt())
// }

// // ensures that the streamer starts from the last processed block
// func TestStreamerStartAtSpecificBlock(t *testing.T) {
// 	// inits the db
// 	dbInstance := db.New()
// 	assert.NoError(t, dbInstance.Init())
// 	assert.NoError(t, dbInstance.Start())
// 	defer dbInstance.Stop()

// 	vscDb := vsc.New(dbInstance)
// 	assert.NoError(t, vscDb.Init())
// 	assert.NoError(t, vscDb.Start())
// 	defer vscDb.Stop()

// 	// creates the streamer with a specific start block
// 	startAtBlock := 1999
// 	filters := []hiveStreamer.FilterFunc{dummyTransferFiler, dummyJsonFilter}

// 	hiveBlocks := hive_blocks.New(vscDb)
// 	streamer := hiveStreamer.New(hiveBlocks, filters, dummyBlockProcesser, &startAtBlock, nil)

// 	// inits the streamer
// 	assert.NoError(t, streamer.Init())

// 	// startBlock should be set to the specified block
// 	assert.Equal(t, 1999, streamer.GetBlockStartedAt())
// }

// func TestFiltering(t *testing.T) {
// 	// defines a sample dummy block with txs and ops
// 	block := hive_blocks.HiveBlock{
// 		Transactions: []hive_blocks.Tx{
// 			{
// 				TransactionID: "tx1",
// 				Operations: []hive_blocks.Op{
// 					{
// 						Type: "transfer_operation",
// 						Value: map[string]interface{}{
// 							"from":   "alice",
// 							"to":     "bob",
// 							"amount": "15",
// 						},
// 					},
// 					{
// 						Type: "vote_operation",
// 						Value: map[string]interface{}{
// 							"voter":  "alice",
// 							"author": "bob",
// 						},
// 					},
// 				},
// 			},
// 			{
// 				TransactionID: "tx2",
// 				Operations: []hive_blocks.Op{
// 					{
// 						Type: "transfer_operation",
// 						Value: map[string]interface{}{
// 							"from":   "bob",
// 							"to":     "alice",
// 							"amount": "25",
// 						},
// 					},
// 					{
// 						Type: "custom_json_operation",
// 						Value: map[string]interface{}{
// 							"json": "hello world",
// 						},
// 					},
// 				},
// 			},
// 		},
// 	}

// 	// use our pre-defined dummy filters
// 	filters := []hiveStreamer.FilterFunc{dummyTransferFiler, dummyJsonFilter}

// 	// simulate our filtering
// 	filteredTxs := []hive_blocks.Tx{}
// 	for _, tx := range block.Transactions {
// 		filteredOps := []hive_blocks.Op{}
// 		for _, op := range tx.Operations {
// 			for _, filter := range filters {
// 				if filter(op) {
// 					filteredOps = append(filteredOps, op)
// 					break
// 				}
// 			}
// 		}
// 		if len(filteredOps) > 0 {
// 			filteredTxs = append(filteredTxs, hive_blocks.Tx{
// 				TransactionID: tx.TransactionID,
// 				Operations:    filteredOps,
// 			})
// 		}
// 	}

// 	// assert that the filtering worked as expected
// 	assert.Equal(t, 2, len(filteredTxs))

// 	// assert values for the first tx
// 	assert.Equal(t, "tx1", filteredTxs[0].TransactionID)
// 	assert.Equal(t, 1, len(filteredTxs[0].Operations))
// 	assert.Equal(t, "transfer_operation", filteredTxs[0].Operations[0].Type)

// 	// assert values for the second tx
// 	assert.Equal(t, "tx2", filteredTxs[1].TransactionID)
// 	assert.Equal(t, 2, len(filteredTxs[1].Operations))

// 	// assert types of the ops
// 	opTypes := []string{
// 		filteredTxs[1].Operations[0].Type,
// 		filteredTxs[1].Operations[1].Type,
// 	}
// 	assert.Contains(t, opTypes, "transfer_operation")
// 	assert.Contains(t, opTypes, "custom_json_operation")
// }

// // generic test case for the streamer
// func TestStreamer(t *testing.T) {
// 	// init the db
// 	dbInstance := db.New()
// 	assert.NoError(t, dbInstance.Init())
// 	assert.NoError(t, dbInstance.Start())
// 	defer dbInstance.Stop()

// 	vscDb := vsc.New(dbInstance)
// 	assert.NoError(t, vscDb.Init())
// 	assert.NoError(t, vscDb.Start())
// 	defer vscDb.Stop()

// 	hiveBlocks := hive_blocks.New(vscDb)

// 	// simple filter
// 	simpleTransferFilter := func(op hive_blocks.Op) bool {
// 		return op.Type == "transfer_operation"
// 	}

// 	// simple process func
// 	simpleProcessFunction := func(block hive_blocks.HiveBlock) error {
// 		// we don't do anything here... but we could
// 		return nil
// 	}

// 	// set a random block to start from
// 	startAtBlock := 200

// 	// create a chan to receive blocks
// 	blockChan := make(chan hive_blocks.HiveBlock, 10)

// 	// create the streamer with the simple filter and process funcs
// 	streamer := hiveStreamer.New(hiveBlocks, []hiveStreamer.FilterFunc{simpleTransferFilter}, simpleProcessFunction, &startAtBlock, blockChan)
// 	assert.NotNil(t, streamer)

// 	// init streamer
// 	err := streamer.Init()
// 	assert.NoError(t, err)

// 	// start streamer
// 	err = streamer.Start()
// 	assert.NoError(t, err)

// 	// use wait group to simulate waiting for some blocks to process
// 	var wg sync.WaitGroup
// 	wg.Add(1)

// 	// use a timeout to avoid hanging if no blocks are streamed
// 	timeout := time.After(5 * time.Second)

// 	// consume blocks from the block chan
// 	go func() {
// 		defer wg.Done()
// 		for i := 0; i < 5; i++ { // process only a few blocks for simplicity
// 			select {
// 			case <-blockChan:
// 				// process block
// 			case <-timeout:
// 				return
// 			}
// 		}
// 		assert.Nil(t, streamer.Stop()) // stop the streamer
// 	}()

// 	// wait for the routine to finish
// 	wg.Wait()

// 	// stop streamer
// 	assert.NoError(t, streamer.Stop())
// }

// func TestContinuallyPollAndStreamPauseContinue(t *testing.T) {
// 	// init the db
// 	dbInstance := db.New()
// 	assert.NoError(t, dbInstance.Init())
// 	assert.NoError(t, dbInstance.Start())
// 	defer dbInstance.Stop()

// 	vscDb := vsc.New(dbInstance)
// 	assert.NoError(t, vscDb.Init())
// 	assert.NoError(t, vscDb.Start())
// 	defer vscDb.Stop()

// 	hiveBlocks := hive_blocks.New(vscDb)

// 	// vars to track the number of blocks processed before and after pause
// 	var numBlocksBeforePause, numBlocksAfterResume int
// 	var pauseCalled, resumeCalled bool

// 	var streamer *hiveStreamer.Streamer

// 	// process func increments counters based on the block number
// 	processBlock := func(block hive_blocks.HiveBlock) error {
// 		// todo: fix this, something is wack
// 		t.Logf("processing block: %d", block.BlockNumber)
// 		if !pauseCalled {
// 			numBlocksBeforePause++
// 			t.Logf("blocks before pause: %d", numBlocksBeforePause)
// 			if numBlocksBeforePause == 5 {
// 				pauseCalled = true
// 				t.Log("pausing streamer")
// 				streamer.Pause()

// 				// resume after a little sleep
// 				go func() {
// 					time.Sleep(1 * time.Second)
// 					t.Log("resuming streamer!")
// 					streamer.Resume()
// 					resumeCalled = true
// 				}()
// 			}
// 		} else if resumeCalled {
// 			numBlocksAfterResume++
// 			t.Logf("blocks after resume: %d", numBlocksAfterResume)
// 			if numBlocksAfterResume == 5 {
// 				t.Log("stopping streamer!")
// 				streamer.Stop()
// 			}
// 		}
// 		return nil
// 	}

// 	// define a non-filtering filter for testing
// 	allOpsFilter := func(op hive_blocks.Op) bool {
// 		return true
// 	}

// 	// start from a known block that has data
// 	startAtBlock := 1232
// 	filters := []hiveStreamer.FilterFunc{allOpsFilter}

// 	// create new streamer
// 	streamer = hiveStreamer.New(hiveBlocks, filters, processBlock, &startAtBlock, nil) // assign streamer
// 	assert.NotNil(t, streamer)
// 	defer streamer.Stop()

// 	// init the streamer
// 	assert.NoError(t, streamer.Init())

// 	// start the streamer
// 	assert.NoError(t, streamer.Start())

// 	// timeout to ensure the test does not hang indefinitely
// 	timeout := time.After(30 * time.Second)
// 	done := make(chan struct{})

// 	// wait for the streamer to complete processing or timeout
// 	go func() {
// 		for {
// 			if numBlocksBeforePause == 5 && numBlocksAfterResume == 5 {
// 				close(done)
// 				return
// 			}
// 			select {
// 			case <-timeout:
// 				t.Log("timeout reached while processing")
// 				streamer.Stop()
// 				close(done)
// 				return
// 			default:
// 				// just continue
// 			}
// 		}
// 	}()

// 	<-done

// 	// verify that the expected number of blocks were processed before pause and after resume
// 	assert.Equal(t, 5, numBlocksBeforePause)
// 	assert.Equal(t, 5, numBlocksAfterResume)
// }
