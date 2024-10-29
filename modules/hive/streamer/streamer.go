package streamer

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"vsc-node/modules/aggregate"
	hiveblocks "vsc-node/modules/db/vsc/hive_blocks"

	"github.com/vsc-eco/hivego"
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
	BlockBatchSize = 10
	// how far behind we're willing to be in blocks from the head before
	// we re-pull the newest batch
	//
	// the code will ignore this if we're more than [blockBatchSize] behind
	AcceptableBlockLag = 5
	// delay between re-polling for the newest information about the chain height
	HeadBlockCheckPollInterval = time.Second * 5
	// even if all predicate funcs say we should keep pulling the next batch of
	// blocks, this is how long we should wait between fetches
	MinTimeBetweenBlockBatchFetches = time.Second * 3
)

// ===== interface implementation =====

var _ aggregate.Plugin = &Streamer{}

// ===== type definitions =====

type FilterFunc func(op hivego.Operation) bool
type ProcessFunction func(block hiveblocks.HiveBlock) error

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

	// retrieves the last processed block
	lastBlock, err := s.hiveBlocks.GetLastProcessedBlock()
	if err != nil {
		return fmt.Errorf("error getting last block: %v", err)
	}

	// determines the starting block based on input or last saved block
	if s.startBlock == nil || *s.startBlock < lastBlock {
		if lastBlock == 0 {
			// no prev blocks processed; start from block 1
			s.startBlock = &[]int{1}[0] // sneaky way to avoid creating a new variable for *int
		} else {
			// continue from the next block after our last processed one
			s.startBlock = &[]int{lastBlock + 1}[0] // sneaky way to avoid creating a new variable for *int
		}
	}
	return nil
}

func (s *Streamer) Start() error {
	s.stopped = make(chan struct{})
	go s.streamBlocks()
	go s.trackHeadHeight()
	return nil // returns error just to satisfy plugin interface
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

			// this case can also trigger when getting the head height fails and it
			// defaults the headHeight to 0, which makes sense
			if currentHeadHeight-localStartBlock <= AcceptableBlockLag {
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
					blk.BlockNumber = *s.startBlock
					err = s.processBlock(&blk)
					if err != nil {
						return
					}
					*s.startBlock++
				}
			}

			// await before re-checking
			time.Sleep(MinTimeBetweenBlockBatchFetches)
		}
	}
}

func (s *Streamer) fetchBlockBatch(startBlock, batchSize int) ([]hivego.Block, error) {
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
			block.BlockNumber = currentBlock
			currentBlock++
			if block.BlockID == "" {
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
	}

	// filter txs within the block
	for _, tx := range block.Transactions {
		// filter the ops within this tx
		filteredTx := hiveblocks.Tx{
			Operations: []hivego.Operation{},
		}
		for _, op := range tx.Operations {
			shouldInclude := true
			for _, filter := range s.filters {
				if !filter(op) {
					shouldInclude = false
					break
				}
			}
			if shouldInclude {
				filteredTx.Operations = append(filteredTx.Operations, op)
			}
		}

		// add the tx if it has any ops that passed the filters
		if len(filteredTx.Operations) > 0 {
			hiveBlock.Transactions = append(hiveBlock.Transactions, filteredTx)
		}
	}

	// store the block with filtered txs
	//
	// even if a block has no txs, we store
	if err := s.hiveBlocks.StoreBlock(&hiveBlock); err != nil {
		return fmt.Errorf("failed to store block: %v", err)
	}

	// calls the process func on the stored block
	if s.process != nil {
		if err := s.process(hiveBlock); err != nil {
			return fmt.Errorf("failed to process block: %v", err)
		}
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
