package streamer

import (
	"context"
	"fmt"
	"sync"
	"time"

	"vsc-node/modules/aggregate"
	hiveblocks "vsc-node/modules/db/vsc/hive_blocks"

	"github.com/vsc-eco/hivego"
	"go.mongodb.org/mongo-driver/bson"
)

// ===== constants =====

const HiveDataSource = "https://api.hive.blog"
const BlockBatchSize = 100

// ===== interface assertions =====

var _ aggregate.Plugin = &Streamer{}

// ===== types =====

// func that takes an operation and returns a boolean, aka: do we want to keep txs with this operation?
type FilterFunc func(op hiveblocks.Op) bool

// func that processes a block
//
// this is called AFTER a block is filtered (and stored/persisted)
type ProcessFunction func(block hiveblocks.HiveBlock) error

// manages stream of blocks from Hive blockchain
type Streamer struct {
	hiveBlocks   hiveblocks.HiveBlocks
	client       *hivego.HiveRpcNode
	startBlock   *int
	ctx          context.Context
	cancel       context.CancelFunc
	filters      []FilterFunc
	process      ProcessFunction
	streamPaused bool
	mtx          sync.Mutex
	stopped      chan struct{}
	BlockChan    chan hiveblocks.HiveBlock
	// this ensures that only one stop is called to not double-close chans and get errors
	stopOnlyOnce sync.Once
}

// ===== streamer core logic =====

// gen a new Streamer
//
// takes in:
// - a list of filters, which are executed in order based on their list
// - a process function, which is called after a block is filtered, and can be used for a caller to do something extra with the block if they desire
// - a startAtBlock, which is the block number to start at (if nil, it will start at the last processed block that is persisted + 1)
// - a blockChan, which is a channel to send blocks to (if nil, blocks are not sent to a channel), this can also be used for the caller to do something extra with the blocks they desire
func New(hiveBlocks hiveblocks.HiveBlocks, filters []FilterFunc, process ProcessFunction, startAtBlock *int, blockChan chan hiveblocks.HiveBlock) *Streamer {
	ctx, cancel := context.WithCancel(context.Background())

	return &Streamer{
		hiveBlocks:   hiveBlocks,
		client:       hivego.NewHiveRpc(HiveDataSource), // use hivego to use pre-built RPC calls
		filters:      filters,
		process:      process,
		ctx:          ctx,
		cancel:       cancel,
		startBlock:   startAtBlock,
		BlockChan:    blockChan,
		streamPaused: false,
		stopped:      nil,
	}
}

// init the streamer
func (s *Streamer) Init() error {
	if s.client == nil {
		return fmt.Errorf("hive client is nil")
	}
	if s.hiveBlocks == nil {
		return fmt.Errorf("hiveblocks is nil")
	}

	if s.startBlock == nil {
		// if we don't specify WHERE we start, we start at the last processed block + 1
		lastProcessedBlock, err := s.hiveBlocks.GetLastProcessedBlock()
		if err != nil {
			return fmt.Errorf("error getting last processed block: %v", err)
		} else {
			lp := lastProcessedBlock + 1
			s.startBlock = &lp
		}
	}

	return nil
}

// start the streamer
func (s *Streamer) Start() error {
	s.stopped = make(chan struct{})
	go s.streamBlocks()
	return nil
}

// internally used to stream blocks
func (s *Streamer) streamBlocks() {
	defer func() {
		if s.stopped != nil {
			close(s.stopped)
		}
	}()

	// continually loop until the ctx is done
	for {
		select {
		case <-s.ctx.Done():
			// stream stopped
			return
		default:
			// if ctx not done, then we check if the stream is paused, if so, we basically await for it to unpause
			if s.GetStreamPaused() {
				time.Sleep(1 * time.Second)
				continue
			}

			// fetch using the start block and the batch size
			blocks, err := s.fetchBlockBatch(*s.startBlock, BlockBatchSize)
			if err != nil {
				// if error, sleep for a bit, then try again
				time.Sleep(3 * time.Second)
				continue
			}

			// for each block that we get
			for _, blk := range blocks {
				select {
				case <-s.ctx.Done():
					// ctx expired DURING batch processing, so just return
					return
				default:
					// process the block
					blk.BlockNumber = *s.startBlock
					err = s.processBlock(&blk)
					if err != nil {
						return // some error processing the block, so we just return
					}
					*s.startBlock++
				}
			}
		}
	}
}

// fetch a batch of blocks based on the argument size
func (s *Streamer) fetchBlockBatch(startBlock, batchSize int) ([]hivego.Block, error) {
	var blocks []hivego.Block
	for i := 0; i < batchSize; i++ {
		select {
		case <-s.ctx.Done():
			// if ctx cancelled, return the blocks we have thus far
			return blocks, fmt.Errorf("fetch block batch cancelled via ctx")
		default:
			// just iter through the batch size and fetch the blocks using the RPC
			// todo: batch this? instead of iterating 1-by-1?
			blockNum := startBlock + i
			block, err := s.client.GetBlock(blockNum)
			if err != nil {
				// some block fetch error, return the blocks we have thus far
				return nil, err
			}

			// if the blk is nil or the block ID is empty, then we wait a bit and try again
			if len(block.BlockID) == 0 {
				time.Sleep(3 * time.Second)
				continue
			}

			// got a good block!! append it to the list
			blocks = append(blocks, block)
		}
	}

	// return the blocks we have thus far
	return blocks, nil
}

// process a block
func (s *Streamer) processBlock(block *hivego.Block) error {
	filteredTxs := []hiveblocks.Tx{}

	txIDs := block.TransactionIds
	transactions := block.Transactions

	for i, tx := range transactions {
		var txID string
		if i < len(txIDs) {
			txID = txIDs[i]
		} else {
			txID = fmt.Sprintf("tx_%d_%d", block.BlockNumber, i)
		}

		filteredOps := []hiveblocks.Op{}

		for _, op := range tx.Operations {
			opType := op.Type
			opValue := op.Value

			for _, filter := range s.filters {
				// if filter says true, we keep the tx
				if filter(hiveblocks.Op{Type: opType, Value: opValue}) {
					var bsonValue bson.Raw
					bsonValue, err := bson.Marshal(opValue)
					if err != nil {
						// some error marshalling the value, so we just skip
						continue
					}

					filteredOps = append(filteredOps, hiveblocks.Op{
						Type:  opType,
						Value: bsonValue,
					})
					break
				}
			}
		}

		// if we have some filtered ops, we append to the filtered tx list
		if len(filteredOps) > 0 {
			filteredTxs = append(filteredTxs, hiveblocks.Tx{
				TransactionID: txID,
				Operations:    filteredOps,
			})
		}
	}

	// if we have some filtered txs, we process the block, else, if the block is empty, we skip (useless block)
	if len(filteredTxs) > 0 {
		hiveBlock := hiveblocks.HiveBlock{
			BlockNumber:  block.BlockNumber,
			BlockID:      block.BlockID,
			Timestamp:    block.Timestamp,
			Transactions: filteredTxs,
		}

		// call block process func
		err := s.process(hiveBlock)
		if err != nil {
			fmt.Printf("error processing block %s: %v\n", block.BlockID, err)
		}

		// if the caller has provided a block channel (acting as a stream), we then send the block along there!
		if s.BlockChan != nil {
			select {
			case s.BlockChan <- hiveBlock:
			case <-s.ctx.Done():
				// ctx expired during the block send, so just return
				return fmt.Errorf("block send cancelled via ctx")
			}
		}

		// store the block
		err = s.hiveBlocks.StoreBlock(&hiveBlock)
		if err != nil {
			return fmt.Errorf("error storing block: %v", err)
		}
	}

	err := s.hiveBlocks.StoreLastProcessedBlock(block.BlockNumber)
	if err != nil {
		fmt.Printf("error updating last processed block: %v\n", err)
	}

	return nil
}

// pause the streamer
func (s *Streamer) Pause() {
	// we lock stuff like this with mutexes just because we technically could
	// have different threads contesting for the same thing
	s.mtx.Lock()
	defer s.mtx.Unlock()
	if !s.streamPaused {
		s.streamPaused = true
	}
}

// resume the streamer
func (s *Streamer) Resume() error {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	select {
	case <-s.ctx.Done():
		return fmt.Errorf("streamer is stopped")
	default:
		if s.streamPaused {
			s.streamPaused = false
		}
	}
	return nil
}

// stop the streamer
func (s *Streamer) Stop() error {
	// having this stopOnlyOnce has prevented so many error cases
	// where we double-close chans and cause errors, it looks weird,
	// but seems to work
	s.stopOnlyOnce.Do(func() {
		s.cancel() // cancel the ctx
		if s.stopped != nil {
			<-s.stopped // wait for the stream to stop
		}
		// close the block chan if it exists
		if s.BlockChan != nil {
			close(s.BlockChan)
			s.BlockChan = nil
		}
	})
	// good! we stopped
	return nil
}

// check if the streamer is paused
func (s *Streamer) GetStreamPaused() bool {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	return s.streamPaused
}

// get the process func we supplied
func (s *Streamer) GetProcessFunc() ProcessFunction {
	return s.process
}

// get all the filters we supplied
func (s *Streamer) GetFilters() []FilterFunc {
	return s.filters
}

// get which block idx the streamer started at
//
// you may want to know this if you didn't provide a start block
// and it started at the last processed block + 1
func (s *Streamer) GetBlockStartedAt() int {
	return *s.startBlock
}
