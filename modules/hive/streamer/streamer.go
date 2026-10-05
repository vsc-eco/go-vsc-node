package streamer

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"vsc-node/lib/vsclog"
	"vsc-node/modules/aggregate"
	hiveblocks "vsc-node/modules/db/vsc/hive_blocks"

	"github.com/chebyrash/promise"
	"github.com/vsc-eco/hivego"
	"go.mongodb.org/mongo-driver/mongo"
)

var vlog = vsclog.Module("streamer")

// ===== block client interface =====

// interface to add generality to block data source to
// aid in mocking this service for unit tests
type BlockClient interface {
	GetDynamicGlobalProps() ([]byte, error)
	GetBlockRange(startBlock int, count int) ([]hivego.Block, error)
	FetchVirtualOps(block int, onlyVirtual bool, includeReversible bool) ([]hivego.VirtualOp, error)
}

// ===== variables =====

// these are not constants because it should be possible (although not likely)
// to modify these values at runtime
var (
	// how many blocks we pull per batch
	BlockBatchSize = uint64(100)
	// how far behind we're willing to be in blocks from the head before
	// we re-pull the newest batch
	//
	// the code will ignore this if we're more than [blockBatchSize] behind
	//
	// @Vaultec says lag should be 0
	AcceptableBlockLag = uint64(0)
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
	DefaultBlockStart = uint64(81614028)
	// db poll interval
	//
	// @Vaultec says 500ms is ideal
	DbPollInterval = time.Millisecond * 100
	// how long StreamReader.Stop waits for the in-flight block to finish
	// processing in the state engine before giving up. Normally a block
	// processes well inside a second; the bound only matters when the
	// current block hangs (e.g. a stuck wasm call). magid's signal
	// handler leans on this drain, and its second-signal / 30s
	// force-exit backstops cover the pathological case.
	StopDrainTimeout = time.Second * 10
)

// ===== StreamReader =====

type StreamReader struct {
	process        ProcessFunction
	getBlockHeight BlockHeightFunction
	ctx            context.Context
	cancel         context.CancelFunc
	// mtx           sync.Mutex
	isPaused      atomic.Bool
	lastProcessed uint64
	lastSaved     *uint64
	headHeight    uint64
	// stopped is closed by pollDb on exit — after the in-flight block has
	// fully processed and the final cursor checkpoint is written. Stop()
	// waits on it so a shutdown never cuts the current block mid-apply.
	stopped      chan struct{}
	hiveBlocks   hiveblocks.HiveBlocks
	stopOnlyOnce sync.Once
	wg           sync.WaitGroup
	startBlock   uint64
	// pollStarted records whether Start() launched the poll loop. Stop()
	// only waits on the drain if it did, else nothing would ever close it.
	pollStarted atomic.Bool
}

// inits a StreamReader with the provided hiveBlocks interface and process function
func NewStreamReader(
	hiveBlocks hiveblocks.HiveBlocks,
	process ProcessFunction,
	getBlockHeight BlockHeightFunction,
	maybeStartBlock ...uint64,
) *StreamReader {
	startBlock := DefaultBlockStart
	if len(maybeStartBlock) > 0 {
		startBlock = maybeStartBlock[0]
	}
	if process == nil {
		process = func(block hiveblocks.HiveBlock, headHeight *uint64) {} // no-op
	}
	vlog.Verbose("stream reader initialized", "startBlock", startBlock)

	ctx, cancel := context.WithCancel(context.Background())
	return &StreamReader{
		process:        process,
		getBlockHeight: getBlockHeight,
		hiveBlocks:     hiveBlocks,
		ctx:            ctx,
		cancel:         cancel,
		stopped:        make(chan struct{}),
		startBlock:     startBlock,
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

	// guard against stale lastProcessed that's ahead of actual stored blocks
	highestBlock, err := s.hiveBlocks.GetHighestBlock()
	if err == nil && highestBlock > 0 && lp > highestBlock {
		vlog.Warn("lastProcessed exceeds highestBlock, resetting", "lastProcessed", lp, "highestBlock", highestBlock)
		lp = highestBlock
	}

	s.lastProcessed = lp
	s.lastSaved = &lp
	return nil
}

// begins the polling loop for the StreamReader
func (s *StreamReader) Start() *promise.Promise[any] {
	// Flag the poll loop as launched synchronously (the promise executor
	// itself may run later on a pool goroutine) so Stop() knows the drain
	// channel will eventually be closed. If Start is never called, Stop
	// skips the drain wait instead of burning StopDrainTimeout.
	s.pollStarted.Store(true)
	return promise.New(func(resolve func(any), reject func(error)) {
		defer inteceptError(reject)
		s.pollDb(reject)
		resolve(nil)
	})
}

// review2 HIGH #78: this previously did `recover()` + `vlog.Warn` only and
// never rejected the promise. A panic in the poll loop therefore killed the
// block pipeline silently — promise neither resolved nor rejected, no
// restart, no health signal. It now rejects so the supervisor observes the
// failure.
func inteceptError(reject func(error)) {
	if myError := recover(); myError != nil {
		vlog.Error("streamer StreamReader panic — rejecting", "err", myError)
		reject(fmt.Errorf("streamer: StreamReader panicked: %v", myError))
	}
}

// polls the database at intervals, processing new blocks as they arrive
func (s *StreamReader) pollDb(fail func(error)) {
	// stopped closes when this function returns — which, on the graceful
	// path, is only after the block feed's cancel drained the listener
	// goroutine, i.e. after the in-flight block fully finished processing.
	// Stop() waits on it.
	defer close(s.stopped)
	ticker := time.NewTicker(1 * time.Second)
	quit := make(chan struct{})
	// lastSavedMtx guards lastSaved: the ticker goroutine below persists it
	// every second while the block listener updates it after each processed
	// block. Both goroutines are created here, so a local mutex suffices.
	var lastSavedMtx sync.Mutex
	go func() {
		for {
			select {
			case <-ticker.C:
				// do stuff
				lastSavedMtx.Lock()
				saved := s.lastSaved
				lastSavedMtx.Unlock()
				if saved != nil {
					if err := s.hiveBlocks.StoreLastProcessedBlock(*saved); err != nil {

					}
				}
			case <-quit:
				ticker.Stop()
				return
			}
		}
	}()
	defer func() {
		quit <- struct{}{}
	}()
	newBlocksProcessed := 0
	processBlock := func(block hiveblocks.HiveBlock, headHeight *uint64) error {
		s.process(block, headHeight)
		// update last processed block

		lastSavedMtx.Lock()
		if s.getBlockHeight == nil {
			s.lastSaved = &block.BlockNumber
		} else {
			//Retrieves the calculated block height
			bh := s.getBlockHeight(block.BlockNumber, s.lastProcessed)
			s.lastSaved = &bh
		}
		lastSavedMtx.Unlock()

		s.lastProcessed = block.BlockNumber

		newBlocksProcessed++

		if newBlocksProcessed > 100 {
			lastSavedMtx.Lock()
			saved := s.lastSaved
			lastSavedMtx.Unlock()
			if saved != nil {
				if err := s.hiveBlocks.StoreLastProcessedBlock(*saved); err != nil {
					return fmt.Errorf("error updating last processed block: %v", err)
				}
				newBlocksProcessed = 0
			}
		}
		return nil
	}
	cancel, errChan := s.hiveBlocks.ListenToBlockUpdates(s.ctx, s.lastProcessed, processBlock)
	select {
	case err := <-errChan:
		// Two-step output to keep the structured prefix line while still
		// rendering the multi-line panic/stack trace correctly. slog's
		// TextHandler escapes \n and \t inside string values, so passing
		// the stack as an "err" attr produces unreadable output. Instead,
		// log the structured "StreamReader stopped" line first, then write
		// the verbatim error+stack to stderr — gated on the same level so
		// disabling Error-level logs for this module suppresses both.
		vlog.Error("StreamReader stopped — listener error", "lastProcessed", s.lastProcessed)
		if vlog.Enabled(context.Background(), slog.LevelError) {
			fmt.Fprintln(os.Stderr, err)
		}
		fail(err)
	case <-s.ctx.Done():
		// Quiesce the feed. ListenToBlockUpdates' cancel drains: it returns
		// only once the listener goroutine has exited, i.e. once the
		// in-flight block's processing (the state engine's ProcessBlock,
		// including all of its DB writes and the cursor bookkeeping above)
		// has fully completed. No new block can start after this point.
		cancel()
	}

	// Final checkpoint: persist the drained height so a restart resumes
	// exactly where the state engine stopped instead of replaying the last
	// blocks. Best-effort — the periodic ticker above usually already
	// stored this value; a failure here only costs a bounded replay.
	lastSavedMtx.Lock()
	saved := s.lastSaved
	lastSavedMtx.Unlock()
	if saved != nil {
		if err := s.hiveBlocks.StoreLastProcessedBlock(*saved); err != nil {
			vlog.Warn("failed to persist final last-processed block", "height", *saved, "err", err)
		}
	}
}

// stops the StreamReader
//
// Drain semantics: Stop cancels the block feed (no new block can start)
// and then waits — bounded by StopDrainTimeout — for pollDb to exit. pollDb
// only exits after ListenToBlockUpdates' cancel drained the listener
// goroutine, i.e. after the in-flight block finished processing in the
// state engine (all DB writes included). On timeout a warning is logged and
// teardown proceeds; magid's second-signal and force-exit backstops remain
// the escape hatch for a genuinely stuck block. Idempotent via stopOnlyOnce
// so both magid's signal handler and the aggregate teardown can call it.
func (s *StreamReader) Stop() error {
	s.stopOnlyOnce.Do(func() {
		s.cancel()
		if !s.pollStarted.Load() {
			// Start never ran: nothing will close the drain channel.
			return
		}
		select {
		case <-s.stopped:
			vlog.Debug("stream reader drained — current block finished processing")
		case <-time.After(StopDrainTimeout):
			vlog.Warn("timed out waiting for the current block to finish processing; proceeding with shutdown")
		}
	})
	return nil
}

// ===== interface implementation =====

var _ aggregate.Plugin = &Streamer{}
var _ aggregate.Plugin = &StreamReader{}

// ===== type definitions =====

type FilterFunc func(tx hivego.Operation, ctx *BlockParams) bool
type VirtualFilterFunc func(vop hivego.VirtualOp) bool
type ProcessFunction func(block hiveblocks.HiveBlock, headHeight *uint64)

// Block height function returns the last block height that should be resumed form
// This is useful for production where there is a replay requirement to get into *now* state
// Or tests...
type BlockHeightFunction func(lastBlock uint64, lastSavedBlk uint64) uint64

type BlockParams struct {
	NeedsVirtualOps bool
	BlockHeight     uint64
}

type Streamer struct {
	hiveBlocks     hiveblocks.HiveBlocks
	client         BlockClient
	startBlock     *uint64
	ctx            context.Context
	cancel         context.CancelFunc
	filters        []FilterFunc
	vFilters       []VirtualFilterFunc
	streamPaused   bool
	mtx            sync.Mutex
	stopOnlyOnce   sync.Once
	headHeight     uint64
	hasFetchedHead bool
	processWg      sync.WaitGroup
	// streamExited/trackerExited close when the stream/track goroutines
	// have fully returned; Stop waits on them so teardown never races the
	// loop's shared-state reads.
	streamExited  chan struct{}
	trackerExited chan struct{}
	started       bool
}

// ===== streamer =====

func NewStreamer(
	blockClient BlockClient,
	hiveBlocks hiveblocks.HiveBlocks,
	filters []FilterFunc,
	vFilters []VirtualFilterFunc,
	startAtBlock *uint64,
) *Streamer {
	ctx, cancel := context.WithCancel(context.Background())
	return &Streamer{
		hiveBlocks:     hiveBlocks,
		client:         blockClient,
		filters:        filters,
		vFilters:       vFilters,
		ctx:            ctx,
		cancel:         cancel,
		startBlock:     startAtBlock,
		streamPaused:   false,
		hasFetchedHead: false,
		processWg:      sync.WaitGroup{},
		stopOnlyOnce:   sync.Once{},
		streamExited:   make(chan struct{}),
		trackerExited:  make(chan struct{}),
	}
}

func (s *Streamer) Init() error {
	if s.client == nil || s.hiveBlocks == nil {
		return fmt.Errorf("client or hiveBlocks not initialized")
	}

	if s.filters == nil {
		s.filters = []FilterFunc{}
	}

	// gets the last processed block
	lastBlock, err := s.hiveBlocks.GetHighestBlock()
	if err != nil {
		if err != mongo.ErrNoDocuments {
			return fmt.Errorf("error getting last block: %v", err)
		}
	}

	// ensures startBlock is either the given startBlock, lastBlock+1, or DefaultBlockStart
	if s.startBlock == nil || *s.startBlock < lastBlock {
		if lastBlock == 0 {
			s.startBlock = &[]uint64{DefaultBlockStart}[0]
		} else {
			s.startBlock = &[]uint64{lastBlock}[0]
		}
	}

	return nil
}

func (s *Streamer) Start() *promise.Promise[any] {
	s.mtx.Lock()
	s.started = true
	s.mtx.Unlock()
	stream := promise.New(func(resolve func(any), reject func(error)) {
		s.streamBlocks()
		reject(fmt.Errorf("streamer: block stream: exited prematurely"))
	})
	tracker := promise.New(func(resolve func(any), reject func(error)) {
		s.trackHeadHeight()
		reject(fmt.Errorf("streamer: head tracker: exited prematurely"))
	})
	return promise.Then(
		promise.All(context.Background(), stream, tracker),
		context.Background(),
		func([]any) (any, error) {
			return nil, nil
		},
	)
}

func updateHead(bc BlockClient) (uint64, error) {
	props, err := bc.GetDynamicGlobalProps()
	if err != nil {
		return 0, fmt.Errorf("failed to get dynamic global properties: %v", err)
	}

	var data struct {
		HeadBlockNumber uint64 `json:"head_block_number"`
	}
	if err := json.Unmarshal(props, &data); err != nil {
		return 0, fmt.Errorf("failed to unmarshal dynamic global properties: %v", err)
	}

	return data.HeadBlockNumber, nil
}

// updates the head height of the streamer at intervals, and since this endpiont is sensitive
// to rate limiting, we apply backoff intervals
func (s *Streamer) trackHeadHeight() {
	if s.trackerExited != nil {
		defer close(s.trackerExited)
	}
	ticker := time.NewTicker(HeadBlockCheckPollIntervalBeforeFirstUpdate)
	defer ticker.Stop()
	var updateLock sync.Mutex
	backoff := HeadBlockCheckPollIntervalBeforeFirstUpdate

	threeSecTicker := time.NewTicker(3 * time.Second)

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-threeSecTicker.C:
			s.mtx.Lock()
			if s.hasFetchedHead {
				s.headHeight++
			}
			s.mtx.Unlock()
		case <-ticker.C:
			if updateLock.TryLock() {
				// unlock immediately since we're not in a nested goroutine
				updateLock.Unlock()

				head, err := updateHead(s.client)
				if err != nil {
					vlog.Error("failed to update head height", "err", err)

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
	// nil-safe: in-package tests may build a bare Streamer literal and run
	// the loop directly without the exit signals.
	if s.streamExited != nil {
		defer close(s.streamExited)
	}
	// last := time.Now()
	for {
		select {
		case <-s.ctx.Done():
			return
		default:
			// snapshot the shared cursor/head under the lock — trackHeadHeight
			// mutates headHeight/hasFetchedHead and the cursor advance below
			// mutates startBlock on other goroutines
			s.mtx.Lock()
			headHeight := s.headHeight
			startBlock := *s.startBlock
			hasFetchedHead := s.hasFetchedHead
			s.mtx.Unlock()

			if s.IsPaused() || !hasFetchedHead {
				time.Sleep(time.Millisecond * 100)

				continue
			}

			if max(headHeight, startBlock)-min(headHeight, startBlock) <= AcceptableBlockLag {
				time.Sleep(time.Millisecond * 100)

				continue
			}

			if startBlock >= headHeight {
				time.Sleep(time.Second)
				continue
			}

			blocks, err := s.fetchBlockBatch(startBlock, min(BlockBatchSize, headHeight-startBlock))
			if err != nil {
				vlog.Warn("error fetching block batch", "err", err)
				time.Sleep(MinTimeBetweenBlockBatchFetches + 3*time.Second)

				continue
			}

			if len(blocks) == 0 {
				vlog.Warn("no blocks fetched")
				time.Sleep(MinTimeBetweenBlockBatchFetches + 3*time.Second)

				continue
			}

			// if not can store, across this async gap, we should skip processing
			if !s.canStore() || len(blocks) == 0 {
				time.Sleep(time.Millisecond * 100)

				continue
			}

			// review2 HIGH #25 follow-up: do NOT advance the cursor
			// before the batch is durably stored. The previous code
			// incremented *s.startBlock and then launched storeBlocks
			// in a detached goroutine, so an in-process failure
			// (FetchVirtualOps RPC error, DB store error, or a
			// pause/stop mid-batch) permanently skipped the batch —
			// the only recovery was a process restart, since Init
			// re-reads GetHighestBlock. The PR's in-process bail at
			// storeBlocks (HIGH #25) is correct, but it only achieves
			// "retried" across restarts while the cursor moves first.
			//
			// Store synchronously and advance the cursor only after
			// success; on failure back off and retry the SAME range
			// in-process. This trades the previous fetch/store
			// pipelining for correctness of fund-bearing block
			// ingest (a follow-up could reintroduce pipelining with
			// an explicit rewind-on-failure high-water mark).
			batchStart := startBlock
			s.processWg.Add(1)
			storeErr := func() error {
				defer s.processWg.Done()
				return s.storeBlocks(blocks)
			}()
			if storeErr != nil {
				if storeErr.Error() != "empty blocks" {
					vlog.Error("processing blocks failed, retrying in-process", "batchStart", batchStart, "err", storeErr)
				}
				// Interruptible backoff: a failing range is retried in
				// place, but ctx cancel (Stop) must not wait out the
				// full backoff.
				select {
				case <-s.ctx.Done():
					return
				case <-time.After(MinTimeBetweenBlockBatchFetches + 3*time.Second):
				}

				continue
			}

			s.mtx.Lock()
			*s.startBlock += uint64(len(blocks))
			s.mtx.Unlock()

			// wait before fetching the next batch whatever min duration that is preset
			time.Sleep(MinTimeBetweenBlockBatchFetches)
		}
	}
}

func (s *Streamer) fetchBlockBatch(startBlock, batchSize uint64) ([]hivego.Block, error) {
	vlog.Debug("fetching block range", "startBlock", startBlock, "endBlock", startBlock+batchSize-1)
	p := promise.New(func(resolve func([]hivego.Block), reject func(error)) {
		blocks, err := s.client.GetBlockRange(int(startBlock), int(batchSize))
		if err != nil {
			reject(err)
			return
		}
		resolve(blocks)
	})

	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()

	blocks, err := p.Await(ctx)
	if blocks == nil || *blocks == nil {
		return make([]hivego.Block, 0), err
	}
	return *blocks, err
}

func (s *Streamer) storeBlocks(blocks []hivego.Block) error {
	hiveBlocks := make([]hiveblocks.HiveBlock, len(blocks))
	for i, block := range blocks {
		// init the filtered block with essential fields
		hiveBlock := hiveblocks.HiveBlock{
			BlockNumber:  uint64(block.BlockNumber),
			BlockID:      block.BlockID,
			Witness:      block.Witness,
			Timestamp:    block.Timestamp,
			Transactions: []hiveblocks.Tx{},
			MerkleRoot:   block.TransactionMerkleRoot,
		}

		needsVirtualOps := false

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
					op.Type = op.Type[:len(op.Type)-10]
				}
				for _, filter := range s.filters {
					// if the streamer is paused or stopped, skip block processing, this
					// fixes some case where the streamer is stopped but some other go routine
					// is still finishing up a cycle of processing
					if !s.canStore() {
						return fmt.Errorf("streamer is paused or stopped")
					}
					blockParams := &BlockParams{
						NeedsVirtualOps: false,
						BlockHeight:     uint64(block.BlockNumber),
					}
					if filter(op, blockParams) {
						if blockParams.NeedsVirtualOps {
							needsVirtualOps = true
						}
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

		if needsVirtualOps {
			vlog.Trace("Pulling virtual ops")
			// review2 HIGH #25: the error was discarded with `_`. On an RPC
			// failure virtualOps would be empty and the block was stored as
			// if it had no virtual ops — permanently dropping deposits /
			// payouts that depend on them. Fail closed: abort this batch so
			// it is retried rather than persisting an incomplete block.
			virtualOps, vopErr := s.client.FetchVirtualOps(int(block.BlockNumber), true, false)
			if vopErr != nil {
				return fmt.Errorf("streamer: FetchVirtualOps block %d: %w", block.BlockNumber, vopErr)
			}
			bbytes, _ := json.Marshal(virtualOps)
			vlog.Trace("virtual ops fetched", "data", string(bbytes))
			filteredOps := make([]hivego.VirtualOp, 0)
			for _, vop := range virtualOps {
				for _, vFilter := range s.vFilters {
					if vFilter(vop) {
						filteredOps = append(filteredOps, vop)
					}
				}
			}
			hiveBlock.VirtualOps = filteredOps
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
	if err := s.hiveBlocks.StoreBlocks(s.headHeight, hiveBlocks...); err != nil {
		if err.Error() == "empty blocks" {
			return nil
		}
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

		// wait for the block processing goroutines with a timeout
		// to ensure we don't wait forever
		stoppedProcessing := make(chan struct{})
		go func() {
			s.processWg.Wait()
			close(stoppedProcessing)
		}()

		select {
		case <-stoppedProcessing:
			vlog.Debug("all processing routines stopped successfully")
		case <-time.After(5 * time.Second):
			vlog.Warn("timeout waiting for processing routines to stop")
		}

		// Wait (bounded) for the stream and head-tracker loops to fully
		// exit so nothing reads the streamer's shared state after Stop
		// returns — teardown and tests rely on that ordering.
		if func() bool {
			s.mtx.Lock()
			defer s.mtx.Unlock()
			return s.started
		}() {
			select {
			case <-s.streamExited:
			case <-time.After(5 * time.Second):
				vlog.Warn("timeout waiting for the block stream loop to stop")
			}
			select {
			case <-s.trackerExited:
			case <-time.After(5 * time.Second):
				vlog.Warn("timeout waiting for the head tracker loop to stop")
			}
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
	return false
}

func (s *Streamer) HeadHeight() uint64 {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	return s.headHeight
}

func (s *Streamer) StartBlock() uint64 {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	return *s.startBlock
}
