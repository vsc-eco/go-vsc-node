package streamer

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"vsc-node/modules/aggregate"
	hiveblocks "vsc-node/modules/db/vsc/hive_blocks"

	"github.com/chebyrash/promise"
	"github.com/vsc-eco/hivego"
	"go.mongodb.org/mongo-driver/mongo"
)

// ===== block client interface =====

// interface to add generality to block data source to
// aid in mocking this service for unit tests
type BlockClient interface {
	GetDynamicGlobalProps() ([]byte, error)
	GetBlockRange(startBlock int, count int) ([]hivego.Block, error)
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
	// before we've updated it once
	HeadBlockCheckPollIntervalBeforeFirstUpdate = time.Millisecond * 1500
	// delay between re-polling for the newest information about the chain height
	// once we've updated it once, since we know it's not going to change much
	//
	// we need this because if we call it too often, this route seems
	// to get rate limited very, very easily
	HeadBlockCheckPollIntervalOnceUpdated = time.Minute * 1
	// maximum backoff interval for fetching the head block number
	HeadBlockMaxBackoffInterval = time.Minute * 5
	// even if all predicate funcs say we should keep pulling the next batch of
	// blocks, this is how long we should wait between fetches
	MinTimeBetweenBlockBatchFetches = time.Millisecond * 1
	// where the hive block streamer starts from by default if nothing
	// has been persisted yet or overridden as a starting point
	DefaultBlockStart = 81614028
	// db poll interval
	//
	// @Vaultec says 500ms is ideal
	DbPollInterval = time.Millisecond * 100
)

// ===== StreamReader =====

type StreamReader struct {
	process ProcessFunction
	ctx     context.Context
	cancel  context.CancelFunc
	// mtx           sync.Mutex
	isPaused      atomic.Bool
	lastProcessed int
	stopped       chan struct{}
	hiveBlocks    hiveblocks.HiveBlocks
	stopOnlyOnce  sync.Once
	wg            sync.WaitGroup
	startBlock    int
}

// inits a StreamReader with the provided hiveBlocks interface and process function
func NewStreamReader(hiveBlocks hiveblocks.HiveBlocks, process ProcessFunction, maybeStartBlock ...int) *StreamReader {
	startBlock := DefaultBlockStart
	fmt.Println("startBlock", startBlock)
	if len(maybeStartBlock) > 0 {
		startBlock = maybeStartBlock[0]
	}
	if process == nil {
		process = func(block hiveblocks.HiveBlock) {} // no-op
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &StreamReader{
		process:    process,
		hiveBlocks: hiveBlocks,
		ctx:        ctx,
		cancel:     cancel,
		startBlock: startBlock,
	}
}

// inits the StreamReader, fetching the last processed block
func (s *StreamReader) Init() error {
	// fetch the last processed block number
	lp, err := s.hiveBlocks.GetLastProcessedBlock()
	if err != nil {
		return fmt.Errorf("error getting last processed block: %v", err)
	}

	if lp < s.startBlock {
		lp = s.startBlock - 1
	}

	s.lastProcessed = lp
	return nil
}

// begins the polling loop for the StreamReader
func (s *StreamReader) Start() *promise.Promise[any] {
	return promise.New(func(resolve func(any), reject func(error)) {
		defer inteceptError()
		s.pollDb(reject)
		resolve(nil)
	})
}

func inteceptError() {
	MyError := recover()
	fmt.Println(MyError)
}

// polls the database at intervals, processing new blocks as they arrive
func (s *StreamReader) pollDb(fail func(error)) {
	newBlocksProcessed := 0
	processBlock := func(block hiveblocks.HiveBlock) error {
		s.process(block)
		// update last processed block
		s.lastProcessed = block.BlockNumber

		newBlocksProcessed++

		if newBlocksProcessed > 100 {
			if err := s.hiveBlocks.StoreLastProcessedBlock(s.lastProcessed); err != nil {
				return fmt.Errorf("error updating last processed block: %v", err)
			}
			newBlocksProcessed = 0
		}
		return nil
	}
	_, errChan := s.hiveBlocks.ListenToBlockUpdates(s.ctx, s.lastProcessed, processBlock)
	err := <-errChan
	fail(err)
}

// stops the StreamReader
func (s *StreamReader) Stop() error {
	s.cancel()
	return nil
}

// ===== interface implementation =====

var _ aggregate.Plugin = &Streamer{}
var _ aggregate.Plugin = &StreamReader{}

// ===== type definitions =====

type FilterFunc func(tx hivego.Operation) bool
type ProcessFunction func(block hiveblocks.HiveBlock)

type Streamer struct {
	hiveBlocks     hiveblocks.HiveBlocks
	client         BlockClient
	startBlock     *int
	ctx            context.Context
	cancel         context.CancelFunc
	filters        []FilterFunc
	streamPaused   bool
	mtx            sync.Mutex
	stopped        chan struct{}
	stopOnlyOnce   sync.Once
	headHeight     int
	hasFetchedHead bool
	wg             sync.WaitGroup
	processWg      sync.WaitGroup
}

// ===== streamer =====

func NewStreamer(blockClient BlockClient, hiveBlocks hiveblocks.HiveBlocks, filters []FilterFunc, startAtBlock *int) *Streamer {
	ctx, cancel := context.WithCancel(context.Background())
	return &Streamer{
		hiveBlocks:     hiveBlocks,
		client:         blockClient,
		filters:        filters,
		ctx:            ctx,
		cancel:         cancel,
		startBlock:     startAtBlock,
		streamPaused:   false,
		stopped:        nil,
		hasFetchedHead: false,
		wg:             sync.WaitGroup{},
		processWg:      sync.WaitGroup{},
		stopOnlyOnce:   sync.Once{},
	}
}

func (s *Streamer) Init() error {
	if s.client == nil || s.hiveBlocks == nil {
		return fmt.Errorf("client or hiveBlocks not initialized")
	}

	if s.filters == nil {
		s.filters = []FilterFunc{}
	}

	// if start block provided (non-nil) then set it as that and return
	if s.startBlock != nil {
		return nil
	}

	// gets the last processed block
	lastBlock, err := s.hiveBlocks.GetHighestBlock()
	if err != nil {
		if err == mongo.ErrNoDocuments {
			// no previous blocks processed, thus start from DefaultBlockStart
			lastBlock = *s.startBlock
		} else {
			return fmt.Errorf("error getting last block: %v", err)
		}
	}

	// if lastBlock is -1, this means that we haven't processed any
	// blocks yet, thus we should start at our default point
	if lastBlock == -1 {
		lastBlock = *s.startBlock
	}

	// ensures startBlock is either the given startBlock, lastBlock+1, or DefaultBlockStart
	if s.startBlock == nil || *s.startBlock < lastBlock {
		if lastBlock == 0 {
			s.startBlock = &[]int{DefaultBlockStart}[0]
		} else {
			s.startBlock = &[]int{lastBlock}[0]
		}
	}

	return nil
}

func (s *Streamer) Start() *promise.Promise[any] {
	s.stopped = make(chan struct{})
	s.wg.Add(2)
	go func() {
		defer s.wg.Done()
		s.streamBlocks()
	}()
	go func() {
		defer s.wg.Done()
		s.trackHeadHeight()
	}()
	return promise.New(func(resolve func(any), reject func(error)) {
		s.wg.Wait()
		resolve(nil)
	})
}

func updateHead(bc BlockClient) (int, error) {
	props, err := bc.GetDynamicGlobalProps()
	if err != nil {
		return 0, fmt.Errorf("failed to get dynamic global properties: %v", err)
	}

	var data struct {
		HeadBlockNumber int `json:"head_block_number"`
	}
	if err := json.Unmarshal(props, &data); err != nil {
		return 0, fmt.Errorf("failed to unmarshal dynamic global properties: %v", err)
	}

	return data.HeadBlockNumber, nil
}

// updates the head height of the streamer at intervals, and since this endpiont is sensitive
// to rate limiting, we apply backoff intervals
func (s *Streamer) trackHeadHeight() {
	ticker := time.NewTicker(HeadBlockCheckPollIntervalBeforeFirstUpdate)
	defer ticker.Stop()
	var updateLock sync.Mutex
	backoff := HeadBlockCheckPollIntervalBeforeFirstUpdate

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			if updateLock.TryLock() {
				// unlock immediately since we're not in a nested goroutine
				updateLock.Unlock()

				head, err := updateHead(s.client)
				if err != nil {
					log.Printf("failed to update head height: %v\n", err)

					// apply backoff with max cap if update fails
					if backoff < HeadBlockMaxBackoffInterval {
						backoff *= 2
						if backoff > HeadBlockMaxBackoffInterval {
							backoff = HeadBlockMaxBackoffInterval
						}
					}
					ticker.Reset(backoff)
					continue
				}

				// on successful head height update
				s.mtx.Lock()
				s.headHeight = head
				if !s.hasFetchedHead {
					s.hasFetchedHead = true // this will then allow the streamer to start
				}
				s.mtx.Unlock()

				// reset backoff to normal interval after successful update
				backoff = HeadBlockCheckPollIntervalOnceUpdated
				ticker.Reset(backoff)
			}
		}
	}
}

func (s *Streamer) streamBlocks() {
	last := time.Now()
	for {
		select {
		case <-s.ctx.Done():
			return
		default:
			if s.IsPaused() || !s.hasFetchedHead {
				time.Sleep(time.Millisecond * 100)

				continue
			}

			if max(s.headHeight, *s.startBlock)-min(s.headHeight, *s.startBlock) <= AcceptableBlockLag {
				time.Sleep(time.Millisecond * 100)

				continue
			}

			fmt.Println("Going to fetch again!", time.Since(last), "block/s", float64(BlockBatchSize)/time.Since(last).Seconds())
			last = time.Now()

			blocks, err := s.fetchBlockBatch(*s.startBlock, BlockBatchSize)
			if err != nil {
				log.Printf("error fetching block batch: %v\n", err)
				time.Sleep(MinTimeBetweenBlockBatchFetches + time.Millisecond*100)

				continue
			}

			// if not can store, across this async gap, we should skip processing
			if !s.canStore() {
				time.Sleep(time.Millisecond * 100)

				continue
			}

			s.mtx.Lock()
			*s.startBlock += len(blocks)
			s.mtx.Unlock()

			// start goroutine to process the blocks
			//
			// this REALLY, REALLY helps speed up the processing of blocks
			s.processWg.Add(1)
			go func() {
				defer s.processWg.Done()
				if err := s.storeBlocks(blocks); err != nil {
					log.Printf("processing blocks failed: %v\n", err)
				}
			}()

			// wait before fetching the next batch whatever min duration that is preset
			time.Sleep(MinTimeBetweenBlockBatchFetches)
		}
	}
}

func (s *Streamer) fetchBlockBatch(startBlock, batchSize int) ([]hivego.Block, error) {
	log.Printf("fetching block range %d-%d\n", startBlock, startBlock+batchSize-1)
	blocks, err := s.client.GetBlockRange(startBlock, batchSize)
	if err != nil {
		return nil, fmt.Errorf("failed to initiate block range fetch: %v", err)
	}

	timeout := time.After(10 * time.Second)

	for {
		select {
		case <-s.ctx.Done():
			return blocks, fmt.Errorf("streamer is stopped")
		default:
			return blocks, nil

		case <-timeout:
			return blocks, fmt.Errorf("timeout waiting for blocks in range starting at %d", startBlock)
		}
	}
}

func (s *Streamer) storeBlocks(blocks []hivego.Block) error {
	hiveBlocks := make([]hiveblocks.HiveBlock, len(blocks))
	for i, block := range blocks {
		// init the filtered block with essential fields
		hiveBlock := hiveblocks.HiveBlock{
			BlockNumber:  block.BlockNumber,
			BlockID:      block.BlockID,
			Timestamp:    block.Timestamp,
			Transactions: []hiveblocks.Tx{},
			MerkleRoot:   block.TransactionMerkleRoot,
		}

		txIds := block.TransactionIds

		// filter txs within the block
		for i, tx := range block.Transactions {
			// filter the ops within this tx
			filteredTx := hiveblocks.Tx{
				Index:         i,
				TransactionID: txIds[i],
				Operations:    []hivego.Operation{},
			}
			shouldInclude := false

			for _, op := range tx.Operations {
				// remove any postfix of "_operation" if it exists from op.Type
				if len(op.Type) > 10 && op.Type[len(op.Type)-10:] == "_operation" {
					if len(op.Type)-10 == 350 {
						println("350")
					}
					op.Type = op.Type[:len(op.Type)-10]
				}
				for _, filter := range s.filters {
					// if the streamer is paused or stopped, skip block processing, this
					// fixes some case where the streamer is stopped but some other go routine
					// is still finishing up a cycle of processing
					if !s.canStore() {
						return fmt.Errorf("streamer is paused or stopped")
					}
					if filter(op) {
						shouldInclude = true
						break
					}
				}

				filteredTx.Operations = append(filteredTx.Operations, op)
			}

			// add the tx if it has any ops that passed the filters
			if shouldInclude {
				hiveBlock.Transactions = append(hiveBlock.Transactions, filteredTx)
			}
		}

		hiveBlocks[i] = hiveBlock
	}

	// if the streamer is paused or stopped, skip block processing, this
	// fixes some case where the streamer is stopped but some other go routine
	// is still finishing up a cycle of processing
	if !s.canStore() {
		return fmt.Errorf("streamer is paused or stopped")
	}
	// store the block with filtered txs
	//
	// even if a block has no txs, we store
	if err := s.hiveBlocks.StoreBlocks(hiveBlocks...); err != nil {
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

func (s *Streamer) canStore() bool {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	return !s.streamPaused && !s.IsStopped()
}

func (s *Streamer) Stop() error {
	s.stopOnlyOnce.Do(func() {
		s.cancel() // cancel context to signal all goroutines to stop

		// wait for the main routines to stop
		s.wg.Wait()

		// wait for the block processing goroutines with a timeout
		// to ensure we don't wait forever
		stoppedProcessing := make(chan struct{})
		go func() {
			s.processWg.Wait()
			close(stoppedProcessing)
		}()

		select {
		case <-stoppedProcessing:
			log.Println("all processing routines stopped successfully")
		case <-time.After(5 * time.Second):
			log.Println("timeout waiting for processing routines to stop")
		}

		if s.stopped != nil {
			close(s.stopped)
		}
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
