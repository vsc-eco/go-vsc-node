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

const (
	HiveDataSource = "https://api.hive.blog"
	BlockBatchSize = 10
)

var _ aggregate.Plugin = &Streamer{}

type FilterFunc func(op hiveblocks.Op) bool
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

func New(hiveBlocks hiveblocks.HiveBlocks, filters []FilterFunc, process ProcessFunction, startAtBlock *int) *Streamer {
	ctx, cancel := context.WithCancel(context.Background())
	return &Streamer{
		hiveBlocks:   hiveBlocks,
		client:       hivego.NewHiveRpc(HiveDataSource),
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

	// determine the starting block if not provided
	if s.startBlock == nil {
		lastBlock, err := s.hiveBlocks.GetLastProcessedBlock()
		if err != nil {
			return fmt.Errorf("error getting last block: %v", err)
		}
		if lastBlock == 0 {
			// no previous blocks processed; default to starting at block 1
			s.startBlock = &[]int{1}[0]
		} else {
			// continue from the next block after the last processed one
			s.startBlock = &[]int{lastBlock + 1}[0]
		}
	}

	fmt.Printf("starting stream from block: %d\n", *s.startBlock)
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
			fmt.Println("Context done, stopping streamBlocks")
			return
		default:
			if s.IsStreamPaused() {
				fmt.Println("Stream is paused")
				time.Sleep(1 * time.Second)
				continue
			}

			// batch of n blocks
			fmt.Printf("Fetching block batch starting from block %d\n", *s.startBlock)
			blocks, err := s.fetchBlockBatch(*s.startBlock, BlockBatchSize)
			if err != nil {
				fmt.Printf("Error fetching block batch: %v\n", err)
				time.Sleep(3 * time.Second)
				continue
			}

			for _, blk := range blocks {
				select {
				case <-s.ctx.Done():
					fmt.Println("Context done during block processing, stopping streamBlocks")
					return
				default:
					blk.BlockNumber = *s.startBlock
					fmt.Printf("Processing block: %d\n", blk.BlockNumber)
					err = s.processBlock(&blk)
					if err != nil {
						fmt.Printf("Error processing block %d: %v\n", blk.BlockNumber, err)
						return
					}
					*s.startBlock++
				}
			}

			// await for 3 seconds before checking for new data
			time.Sleep(3 * time.Second)
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
				fmt.Printf("Empty block ID for block number %d, skipping\n", block.BlockNumber)
				continue
			}
			fmt.Printf("Received block %d with ID %s\n", block.BlockNumber, block.BlockID)
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
	hiveBlock := hiveblocks.HiveBlock{
		BlockNumber:  block.BlockNumber,
		BlockID:      block.BlockID,
		Timestamp:    block.Timestamp,
		Transactions: []hiveblocks.Tx{}, // init with empty txs if necessary
	}

	// directly store the block instead of relying on `s.process`, aka, we store and let the user just "do fun stuff"
	if err := s.hiveBlocks.StoreBlock(&hiveBlock); err != nil {
		return fmt.Errorf("failed to store block: %v", err)
	}

	fmt.Printf("Stored block: %d\n", hiveBlock.BlockNumber)
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
