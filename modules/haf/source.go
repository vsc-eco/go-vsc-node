package haf

import (
	"context"
	"fmt"
	"runtime/debug"
	"time"

	"vsc-node/lib/vsclog"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/db/vsc/hive_blocks"
	"vsc-node/modules/hive/streamer"

	"github.com/chebyrash/promise"
	"go.mongodb.org/mongo-driver/mongo"
)

var vlog = vsclog.Module("haf")

// how many blocks are pulled from HAF per batch
var BlockBatchSize = uint64(10000)

// delay between polls once caught up with the irreversible head
var PollInterval = time.Second

// delay before retrying a failed batch (the same range is always retried,
// never skipped)
var FetchRetryDelay = time.Second * 3

// attempts for the one-shot read methods (GetBlock/FetchStoredBlocks)
var readAttempts = 3

// Source implements hive_blocks.HiveBlocks backed by a HAF database.
//
// Reads come from HAF's hive.irreversible_*_view; mongo keeps only what HAF
// does not have: the {block_number, timestamp} shim for every height — the
// graphql/consensus timestamp joins key on referenced heights (TSS session
// blocks, gateway anchors) that need not carry any tracked operation — plus
// the metadata doc (processing cursor, reindex id). Heights with no relevant
// transactions are stored shim-only and still delivered to listeners so every
// height ticks block processing.
type Source struct {
	client   blockFetcher
	store    hive_blocks.HiveBlocks
	filters  []streamer.FilterFunc
	vFilters []streamer.VirtualFilterFunc
	// startFloor is the lowest height this node ingests (the streamer start
	// height). Reads below it behave exactly like the Hive API streamer's
	// store, which never ingested them. This is consensus-critical for
	// computeSchedule's seed: a height that exists in HAF but was never
	// stored on API-mode nodes must still report "not found", or replay
	// would derive different witness schedules than the rest of the network.
	startFloor uint64

	ctx    context.Context
	cancel context.CancelFunc
}

var _ hive_blocks.HiveBlocks = &Source{}
var _ aggregate.Plugin = &Source{}

// New connects to the HAF database at connString (a PostgreSQL connection
// string) and wraps the given mongo-backed store for shim and cursor writes.
func New(
	connString string,
	store hive_blocks.HiveBlocks,
	filters []streamer.FilterFunc,
	vFilters []streamer.VirtualFilterFunc,
	startFloor uint64,
) (*Source, error) {
	client, err := NewClient(context.Background(), connString)
	if err != nil {
		return nil, err
	}
	return newSource(client, store, filters, vFilters, startFloor), nil
}

func newSource(
	client blockFetcher,
	store hive_blocks.HiveBlocks,
	filters []streamer.FilterFunc,
	vFilters []streamer.VirtualFilterFunc,
	startFloor uint64,
) *Source {
	ctx, cancel := context.WithCancel(context.Background())
	return &Source{
		client:     client,
		store:      store,
		filters:    filters,
		vFilters:   vFilters,
		startFloor: startFloor,
		ctx:        ctx,
		cancel:     cancel,
	}
}

// ===== plugin lifecycle =====

func (s *Source) Init() error {
	if s.client == nil || s.store == nil {
		return fmt.Errorf("haf: client or store not initialized")
	}
	return nil
}

func (s *Source) Start() *promise.Promise[any] {
	return promise.New(func(resolve func(any), reject func(error)) {
		resolve(nil)
	})
}

func (s *Source) Stop() error {
	s.cancel()
	if c, ok := s.client.(*Client); ok {
		c.Close()
	}
	return nil
}

// ===== HAF-backed reads =====

// StoreBlocks persists the {block_number, timestamp} shim every height needs
// for graphql/consensus timestamp joins — everything else about these blocks
// lives in HAF. Empty heights MUST be stored too: joins key on arbitrary
// referenced heights (a TSS commitment's session block_height, a gateway
// tx's anchor height) that need not carry any tracked operation, and
// hive_blocks.GetAggTimestampPipeline's $lookup+$unwind silently drops rows
// whose shim is absent.
func (s *Source) StoreBlocks(headBlock uint64, blocks ...hive_blocks.HiveBlock) error {
	shims := make([]hive_blocks.HiveBlock, 0, len(blocks))
	for _, b := range blocks {
		shims = append(shims, hive_blocks.HiveBlock{
			BlockNumber: b.BlockNumber,
			Timestamp:   b.Timestamp,
		})
	}
	if len(shims) == 0 {
		// no blocks in the batch; keep the head height fresh anyway
		return s.store.SetMetadata(hive_blocks.Document{
			Type:       hive_blocks.DocumentTypeMetadata,
			HeadHeight: &headBlock,
		})
	}
	return s.store.StoreBlocks(headBlock, shims...)
}

// FetchStoredBlocks assembles every block in the range from HAF — empty
// heights included, since pendulum warmup and double-sign rehydration iterate
// per height.
func (s *Source) FetchStoredBlocks(startBlock uint64, endBlock uint64) ([]hive_blocks.HiveBlock, error) {
	if endBlock < s.startFloor {
		return []hive_blocks.HiveBlock{}, nil
	}
	if startBlock < s.startFloor {
		startBlock = s.startFloor
	}
	headers, ops, err := s.fetchRange(startBlock, endBlock)
	if err != nil {
		return nil, err
	}
	return AssembleBlocks(headers, ops, s.filters, s.vFilters)
}

// GetBlock assembles a single block from HAF. Heights below the ingestion
// floor report mongo.ErrNoDocuments exactly like the API-mode store.
func (s *Source) GetBlock(blockNum uint64) (hive_blocks.HiveBlock, error) {
	if blockNum < s.startFloor {
		return hive_blocks.HiveBlock{}, mongo.ErrNoDocuments
	}
	headers, ops, err := s.fetchRange(blockNum, blockNum)
	if err != nil {
		return hive_blocks.HiveBlock{}, err
	}
	if len(headers) == 0 {
		return hive_blocks.HiveBlock{}, mongo.ErrNoDocuments
	}
	blocks, err := AssembleBlocks(headers, ops, s.filters, s.vFilters)
	if err != nil {
		return hive_blocks.HiveBlock{}, err
	}
	return blocks[0], nil
}

// fetchRange retries transient read errors; the one-shot read methods cannot
// fail over to a second source, so a blip must not surface as "block missing"
// (computeSchedule treats any GetBlock error as absent and falls back to the
// default schedule seed).
func (s *Source) fetchRange(start, end uint64) ([]BlockRow, []OpRow, error) {
	var lastErr error
	for attempt := 0; attempt < readAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-s.ctx.Done():
				return nil, nil, s.ctx.Err()
			case <-time.After(FetchRetryDelay):
			}
		}
		ctx, cancel := context.WithTimeout(s.ctx, 60*time.Second)
		headers, ops, err := s.client.FetchRange(ctx, start, end)
		cancel()
		if err == nil {
			return headers, ops, nil
		}
		lastErr = err
	}
	return nil, nil, lastErr
}

// GetHighestBlock reports the irreversible head. It doubles as the chain head
// for txpool RC checks and the graphql head queries; the mongo shim index
// cannot (it only holds relevant blocks).
func (s *Source) GetHighestBlock() (uint64, error) {
	ctx, cancel := context.WithTimeout(s.ctx, 15*time.Second)
	defer cancel()
	return s.client.Head(ctx)
}

// ===== mongo-delegated cursor/metadata =====

func (s *Source) ClearBlocks() error {
	return s.store.ClearBlocks()
}

func (s *Source) StoreLastProcessedBlock(blockNumber uint64) error {
	return s.store.StoreLastProcessedBlock(blockNumber)
}

func (s *Source) GetLastProcessedBlock() (uint64, error) {
	return s.store.GetLastProcessedBlock()
}

func (s *Source) SetMetadata(doc hive_blocks.Document) error {
	return s.store.SetMetadata(doc)
}

func (s *Source) GetMetadata() (hive_blocks.Document, error) {
	return s.store.GetMetadata()
}

// ===== pull loop =====

// ListenToBlockUpdates pulls blocks from HAF starting at startBlock
// (inclusive) and feeds them to listener — every height, empty blocks
// included. Shims for relevant blocks are written before delivery so
// graphql timestamp joins never race block processing. Fetch failures retry
// the same range (a range is never skipped); a listener error is fatal and
// surfaces on the error channel.
//
// Shutdown contract: the returned CancelFunc signals the feed to stop AND
// blocks until the loop goroutine has fully exited. Because listener calls
// (e.g. StateEngine.ProcessBlock) run synchronously inside the loop, a
// completed cancel guarantees the in-flight block finished processing —
// including its DB writes — and that no new listener call can ever start.
// This is the drain point magid's SIGINT/SIGTERM handler relies on to never
// cut a block mid-processing.
func (s *Source) ListenToBlockUpdates(
	ctx context.Context,
	startBlock uint64,
	listener func(block hive_blocks.HiveBlock, headHeight *uint64) error,
) (context.CancelFunc, <-chan error) {
	ctx, cancel := context.WithCancel(ctx)
	errChan := make(chan error)
	done := make(chan struct{})
	next := startBlock

	fail := func(err error) {
		select {
		case errChan <- err:
		case <-ctx.Done():
		}
	}
	stopped := func() bool {
		select {
		case <-ctx.Done():
			return true
		case <-s.ctx.Done():
			return true
		default:
			return false
		}
	}
	sleep := func(d time.Duration) {
		select {
		case <-ctx.Done():
		case <-s.ctx.Done():
		case <-time.After(d):
		}
	}

	go func() {
		// close(done) is registered FIRST so it runs LAST (defers are LIFO):
		// nobody waiting on done ever observes the exit before the
		// panic-recovery below has had its chance to fail.
		defer close(done)
		// A panic in the loop or listener is fatal-but-graceful: surface it
		// on errChan (mirroring the mongo listener) instead of killing the
		// process silently.
		defer func() {
			if r := recover(); r != nil {
				fail(fmt.Errorf("haf: panic in block listener at block %d: %v\n%s", next, r, debug.Stack()))
			}
		}()
		for !stopped() {
			head, err := s.client.Head(ctx)
			if err != nil {
				fail(fmt.Errorf("haf: head: %w", err))
				return
			}
			if next < s.startFloor {
				next = s.startFloor
			}
			if next > head {
				sleep(PollInterval)
				continue
			}

			end := min(next+BlockBatchSize-1, head)
			headers, ops, err := s.client.FetchRange(ctx, next, end)
			var blocks []hive_blocks.HiveBlock
			if err == nil {
				blocks, err = AssembleBlocks(headers, ops, s.filters, s.vFilters)
			}
			if err != nil {
				// fail closed: never skip a range, retry it in place
				sleep(FetchRetryDelay)
				continue
			}

			// Deliver the contiguous run starting at next. Heights are
			// contiguous in HAF by construction, but if a hole ever appears
			// (e.g. HAF starts above the node's start height), the remainder
			// is retried rather than silently tick-skipped.
			delivered := 0
			for _, block := range blocks {
				if block.BlockNumber != next+uint64(delivered) {
					break
				}
				delivered++
			}
			if delivered == 0 {
				vlog.Error("no blocks available at height — is psql-first-block at or below the node start height? retrying", "height", next, "head", head)
				sleep(FetchRetryDelay)
				continue
			}
			blocks = blocks[:delivered]

			// shims first: graphql timestamp joins must never observe a
			// processed height without its timestamp
			if err := s.StoreBlocks(head, blocks...); err != nil {
				sleep(FetchRetryDelay)
				continue
			}
			for _, block := range blocks {
				if stopped() {
					return
				}
				hh := head
				if err := listener(block, &hh); err != nil {
					fail(err)
					return
				}
			}
			next += uint64(delivered)
		}
	}()
	// cancel-drain: after canceling the feed, wait for the goroutine (and
	// therefore any in-flight listener call) to have returned. Another
	// canceler (Source.Stop via s.ctx) also unblocks the wait for free,
	// since the loop exits either way.
	return func() {
		cancel()
		<-done
	}, errChan
}
