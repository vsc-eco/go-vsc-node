package streamer

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"sync"
	"time"

	"vsc-node/modules/aggregate"
	hiveblocks "vsc-node/modules/db/vsc/hive_blocks"

	"github.com/vsc-eco/hivego"
	"go.mongodb.org/mongo-driver/mongo"
)

// ===== block client interface =====

// interface to add generality to block data source to
// aid in mocking this service for unit tests
type BlockClient interface {
	GetDynamicGlobalProps() ([]byte, error)
	GetBlockRange(startBlock int, count int) (<-chan hivego.Block, error)
}

// ===== variables =====

// these are not constants because it should be possible (although not likely)
// to modify these values at runtime
var (
	// how many blocks we pull per batch
	BlockBatchSize = 100
	// how far behind we're willing to be in blocks from the head before
	// we re-pull the newest batch
	//
	// the code will ignore this if we're more than [blockBatchSize] behind
	//
	// @Vaultec says lag should be 0
	AcceptableBlockLag = 0
	// delay between re-polling for the newest information about the chain height
	HeadBlockCheckPollInterval = time.Millisecond * 1500
	// even if all predicate funcs say we should keep pulling the next batch of
	// blocks, this is how long we should wait between fetches
	MinTimeBetweenBlockBatchFetches = time.Millisecond * 1500
	// where the hive block streamer starts from by default if nothing
	// has been persisted yet or overridden as a starting point
	DefaultBlockStart = 81614028
	// db poll interval
	//
	// @Vaultec says 500ms is ideal
	DbPollInterval = time.Millisecond * 500
)

// ===== interface implementation =====

var _ aggregate.Plugin = &Streamer{}

// ===== type definitions =====

type FilterFunc func(tx map[string]interface{}) bool
type ProcessFunction func(block hiveblocks.HiveBlock)

type Streamer struct {
	hiveBlocks   hiveblocks.HiveBlocks
	client       BlockClient
	startBlock   *int
	ctx          context.Context
	cancel       context.CancelFunc
	filters      []FilterFunc
	process      ProcessFunction
	streamPaused bool
	mtx          sync.Mutex
	stopped      chan struct{}
	stopOnlyOnce sync.Once
	headHeight   int
}

// ===== streamer =====

func New(blockClient BlockClient, hiveBlocks hiveblocks.HiveBlocks, filters []FilterFunc, process ProcessFunction, startAtBlock *int) *Streamer {
	ctx, cancel := context.WithCancel(context.Background())
	return &Streamer{
		hiveBlocks:   hiveBlocks,
		client:       blockClient,
		filters:      filters,
		process:      process,
		ctx:          ctx,
		cancel:       cancel,
		startBlock:   startAtBlock,
		streamPaused: false,
		stopped:      nil,
	}
}

func (s *Streamer) Init() error {
	if s.client == nil || s.hiveBlocks == nil {
		return fmt.Errorf("client or hiveBlocks not initialized")
	}

	// if start block provided (non-nil) then set it as that and return
	if s.startBlock != nil {
		return nil
	}

	// gets the last processed block
	lastBlock, err := s.hiveBlocks.GetLastProcessedBlock(context.Background())
	if err != nil {
		if err == mongo.ErrNoDocuments {
			// no previous blocks processed, thus start from DefaultBlockStart
			lastBlock = DefaultBlockStart
		} else {
			return fmt.Errorf("error getting last block: %v", err)
		}
	}

	// ensures startBlock is either the given startBlock, lastBlock+1, or DefaultBlockStart
	if s.startBlock == nil || *s.startBlock < lastBlock {
		if lastBlock == 0 {
			s.startBlock = &[]int{DefaultBlockStart}[0]
		} else {
			s.startBlock = &[]int{lastBlock + 1}[0]
		}
	}
	return nil
}

func (s *Streamer) Start() error {
	s.stopped = make(chan struct{})
	go s.streamBlocks()
	go s.trackHeadHeight()
	go s.pollDb()
	return nil // returns error just to satisfy plugin interface
}

func (s *Streamer) pollDb() {
	ticker := time.NewTicker(DbPollInterval)
	defer ticker.Stop()

	// what was the last block we processed?
	lastProcessedBlock := *s.startBlock - 1

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			blocks, err := s.hiveBlocks.FetchStoredBlocks(context.Background(), lastProcessedBlock+1, lastProcessedBlock+10)
			if err != nil {
				continue
			}

			for _, block := range blocks {
				s.process(block)
				lastProcessedBlock = block.BlockNumber
			}
		}
	}
}

// trackHeadHeight periodically updates the head block height
func (s *Streamer) trackHeadHeight() {
	ticker := time.NewTicker(HeadBlockCheckPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			props, err := s.client.GetDynamicGlobalProps()
			if err != nil {
				continue
			}

			var data map[string]interface{}
			if err := json.Unmarshal(props, &data); err != nil {
				continue
			}

			// get the latest head block number
			if height, ok := data["head_block_number"].(float64); ok {
				s.mtx.Lock()
				s.headHeight = int(height)
				s.mtx.Unlock()
			}
		}
	}
}

func (s *Streamer) streamBlocks() {
	defer func() {
		if s.stopped != nil {
			close(s.stopped)
		}
	}()

	for {
		select {
		case <-s.ctx.Done():
			return
		default:
			if s.IsPaused() {
				continue
			}

			// check if we're within the acceptable block lag
			s.mtx.Lock()
			currentHeadHeight := s.headHeight
			localStartBlock := *s.startBlock
			s.mtx.Unlock()

			// if we're within the acceptable block lag, we don't need to fetch more blocks
			if int(math.Abs(float64(currentHeadHeight-localStartBlock))) <= AcceptableBlockLag {
				continue
			}

			// batch of n blocks
			blocks, err := s.fetchBlockBatch(*s.startBlock, BlockBatchSize)
			if err != nil {
				time.Sleep(3 * time.Second)
				continue // we can retry after a short delay in this case
			}
			for _, blk := range blocks {
				select {
				case <-s.ctx.Done():
					return
				default:
					// inc startBlock before processing to ensure progress
					*s.startBlock = blk.BlockNumber + 1

					// process block
					err := s.processBlock(&blk)
					if err != nil {
						log.Printf("error processing block %d: %v\n", blk.BlockNumber, err)
						continue // continue to the next block even if there's an error
					}
				}
			}

			// await before re-checking
			time.Sleep(MinTimeBetweenBlockBatchFetches)
		}
	}
}

func (s *Streamer) fetchBlockBatch(startBlock, batchSize int) ([]hivego.Block, error) {
	log.Printf("fetching block range %d-%d\n", startBlock, startBlock+batchSize-1)
	blockChan, err := s.client.GetBlockRange(startBlock, batchSize)
	if err != nil {
		return nil, fmt.Errorf("failed to initiate block range fetch: %v", err)
	}

	var blocks []hivego.Block
	timeout := time.After(10 * time.Second)
	currentBlock := startBlock

	for {
		select {
		case block, ok := <-blockChan:
			if !ok {
				return blocks, nil
			}
			// set the block number directly here to ensure it isn’t zero
			//
			// we know this value is correct because we’re iterating over the range
			block.BlockNumber = currentBlock
			currentBlock++
			if block.BlockID == "" {
				log.Printf("empty block ID at block %d, skipping\n", block.BlockNumber)
				// sanity check: if empty block ID => we assume "bad block"
				continue
			}
			blocks = append(blocks, block)

			if len(blocks) >= batchSize {
				return blocks, nil
			}

		case <-timeout:
			return blocks, fmt.Errorf("timeout waiting for blocks in range starting at %d", startBlock)
		}
	}
}

func (s *Streamer) processBlock(block *hivego.Block) error {
	// init the filtered block with essential fields
	hiveBlock := hiveblocks.HiveBlock{
		BlockNumber:  block.BlockNumber,
		BlockID:      block.BlockID,
		Timestamp:    block.Timestamp,
		Transactions: []hiveblocks.Tx{},
		MerkleRoot:   block.TransactionMerkleRoot,
	}

	txIds := append([]string{}, block.TransactionIds...)

	// filter txs within the block
	for i, tx := range block.Transactions {
		// filter the ops within this tx
		filteredTx := hiveblocks.Tx{
			TransactionID: txIds[i],
			Operations:    []map[string]interface{}{},
		}
		for _, op := range tx.Operations {
			shouldInclude := true
			for _, filter := range s.filters {
				if !filter(op.Value) {
					shouldInclude = false
					break
				}
			}
			if shouldInclude {
				filteredTx.Operations = append(filteredTx.Operations, op.Value)
			}
		}

		// add the tx if it has any ops that passed the filters
		if len(filteredTx.Operations) > 0 {
			hiveBlock.Transactions = append(hiveBlock.Transactions, filteredTx)
		}
	}

	// if the streamer is paused or stopped, skip block processing, this
	// fixes some case where the streamer is stopped but some other go routine
	// is still finishing up a cycle of processing
	if !s.canProcess() {
		return fmt.Errorf("streamer is paused or stopped")
	}
	// store the block with filtered txs
	//
	// even if a block has no txs, we store
	if err := s.hiveBlocks.StoreBlock(context.Background(), &hiveBlock); err != nil {
		return fmt.Errorf("failed to store block: %v", err)
	}

	return nil
}

func (s *Streamer) Pause() {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	s.streamPaused = true
}

func (s *Streamer) Resume() error {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	select {
	case <-s.ctx.Done():
		return fmt.Errorf("streamer is stopped")
	default:
		s.streamPaused = false
		return nil
	}
}

func (s *Streamer) canProcess() bool {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	return !s.streamPaused && !s.IsStopped()
}

func (s *Streamer) Stop() error {
	s.stopOnlyOnce.Do(func() {
		s.cancel()
		<-s.stopped
	})
	return nil
}

func (s *Streamer) IsPaused() bool {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	return s.streamPaused
}

func (s *Streamer) IsStopped() bool {
	select {
	case <-s.stopped:
		return true
	default:
		return false
	}
}

func (s *Streamer) HeadHeight() int {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	return s.headHeight
}

func (s *Streamer) StartBlock() int {
	return *s.startBlock
}
