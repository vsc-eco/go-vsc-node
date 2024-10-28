package streamer

import (
	"context"
	"fmt"
	"sync"
	"time"

	"vsc-node/modules/aggregate"
	hiveblocks "vsc-node/modules/db/vsc/hive_blocks"

	"github.com/vsc-eco/hivego"
)

// ===== constants =====

const (
	hiveDataSource = "https://api.hive.blog"
	blockBatchSize = 10
	pollInterval   = time.Second * 5
)

// ===== interface implementation =====

var _ aggregate.Plugin = &Streamer{}

// ===== type definitions =====

type FilterFunc func(op hivego.Operation) bool
type ProcessFunction func(block hiveblocks.HiveBlock) error

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
	stopOnlyOnce sync.Once
}

// ===== streamer =====

func New(hiveBlocks hiveblocks.HiveBlocks, filters []FilterFunc, process ProcessFunction, startAtBlock *int) *Streamer {
	ctx, cancel := context.WithCancel(context.Background())
	return &Streamer{
		hiveBlocks:   hiveBlocks,
		client:       hivego.NewHiveRpc(hiveDataSource),
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
	return nil
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
			if s.IsStreamPaused() {
				// retry after a second to check if unpaused
				time.Sleep(1 * time.Second)
				continue
			}

			// batch of n blocks
			blocks, err := s.fetchBlockBatch(*s.startBlock, blockBatchSize)
			if err != nil {
				time.Sleep(3 * time.Second)
				continue
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
			time.Sleep(pollInterval)
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

func (s *Streamer) IsStreamPaused() bool {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	return s.streamPaused
}
