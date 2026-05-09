package state_engine

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"vsc-node/lib/datalayer"
	"vsc-node/lib/vsclog"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/common/common_types"
	"vsc-node/modules/db/vsc/hive_blocks"

	"github.com/chebyrash/promise"
	"github.com/ipfs/go-cid"
)

var _ aggregate.Plugin = (*BlockPrefetcher)(nil)

var prefetchLog = vsclog.Module("se-prefetch")

// prefetchFetchTimeout caps how long a single GetDag may block. Without it, a
// CID no peer is serving would pin a worker forever — eight bad CIDs would
// drain the worker pool and silently kill prefetching for the rest of the run.
const prefetchFetchTimeout = 60 * time.Second

// BlockPrefetcher scans upcoming hive blocks for vsc.produce_block ops,
// extracts the referenced VSC block CIDs, and issues concurrent IPFS fetches
// to populate the local blockstore before the state engine processes them.
//
// During reindex this turns sequential bitswap fetches into a pipelined
// pattern, which is the dominant cost on a cold blockstore.
type BlockPrefetcher struct {
	hiveBlocks  hive_blocks.HiveBlocks
	da          *datalayer.DataLayer
	blockStatus common_types.BlockStatusGetter

	lookahead   uint64
	parallelism int
	scanInterval time.Duration

	workQueue chan cid.Cid

	seenMu sync.Mutex
	seen   map[string]uint64 // cid string → hive block height it was queued for

	statsMu      sync.Mutex
	totalQueued  uint64
	totalFetched uint64
	totalErrored uint64
	totalTimeout uint64
	totalFetchNs int64

	ctx       context.Context
	cancel    context.CancelFunc
	workersWg sync.WaitGroup
	scanWg    sync.WaitGroup
}

// NewPrefetcher constructs a BlockPrefetcher. parallelism=0 disables it.
func NewPrefetcher(
	hiveBlocks hive_blocks.HiveBlocks,
	da *datalayer.DataLayer,
	blockStatus common_types.BlockStatusGetter,
	lookahead uint64,
	parallelism int,
) *BlockPrefetcher {
	return &BlockPrefetcher{
		hiveBlocks:   hiveBlocks,
		da:           da,
		blockStatus:  blockStatus,
		lookahead:    lookahead,
		parallelism:  parallelism,
		scanInterval: time.Second,
		seen:         make(map[string]uint64),
	}
}

// Init implements aggregate.Plugin.
func (p *BlockPrefetcher) Init() error { return nil }

// Start implements aggregate.Plugin. No-op when parallelism=0.
func (p *BlockPrefetcher) Start() *promise.Promise[any] {
	return promise.New(func(resolve func(any), reject func(error)) {
		if p.parallelism <= 0 {
			prefetchLog.Info("prefetch disabled", "parallelism", p.parallelism)
			resolve(nil)
			return
		}
		p.ctx, p.cancel = context.WithCancel(context.Background())
		p.workQueue = make(chan cid.Cid, p.parallelism*4)

		p.scanWg.Add(1)
		go p.scanLoop()

		for i := 0; i < p.parallelism; i++ {
			p.workersWg.Add(1)
			go p.fetchWorker(i)
		}
		go p.statsLoop()
		prefetchLog.Info("prefetch started", "lookahead", p.lookahead, "parallelism", p.parallelism)
		resolve(nil)
	})
}

// statsLoop emits a periodic summary at info level so prefetcher activity is
// visible without enabling verbose logging.
func (p *BlockPrefetcher) statsLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			p.statsMu.Lock()
			queued := p.totalQueued
			fetched := p.totalFetched
			errored := p.totalErrored
			timedOut := p.totalTimeout
			ns := p.totalFetchNs
			p.statsMu.Unlock()
			var avgMs int64
			if fetched > 0 {
				avgMs = (ns / int64(fetched)) / int64(time.Millisecond)
			}
			prefetchLog.Info("prefetch stats",
				"queued", queued,
				"fetched", fetched,
				"errored", errored,
				"timeout", timedOut,
				"avg_fetch_ms", avgMs,
				"queue_depth", len(p.workQueue),
				"current_bh", p.blockStatus.BlockHeight(),
			)
		}
	}
}

// Stop implements aggregate.Plugin.
func (p *BlockPrefetcher) Stop() error {
	if p.cancel == nil {
		return nil
	}
	p.cancel()
	p.scanWg.Wait()
	close(p.workQueue)
	p.workersWg.Wait()
	return nil
}

// scanLoop periodically reads upcoming hive blocks and queues produce_block CIDs.
func (p *BlockPrefetcher) scanLoop() {
	defer p.scanWg.Done()

	ticker := time.NewTicker(p.scanInterval)
	defer ticker.Stop()

	// Run once immediately so prefetch begins without waiting for the first tick.
	p.scanOnce()

	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			p.scanOnce()
		}
	}
}

// scanOnce reads the lookahead window and queues produce_block CIDs.
func (p *BlockPrefetcher) scanOnce() {
	current := p.blockStatus.BlockHeight()
	if current == 0 {
		// State engine hasn't started yet; try again next tick.
		prefetchLog.Verbose("scan skipped: BlockHeight=0")
		return
	}
	startBh := current + 1
	endBh := current + p.lookahead

	highest, err := p.hiveBlocks.GetHighestBlock()
	if err != nil {
		prefetchLog.Warn("GetHighestBlock failed", "err", err)
		return
	}
	if endBh > highest {
		endBh = highest
	}
	if startBh > endBh {
		prefetchLog.Verbose("scan caught up with hive_blocks tip", "current", current, "highest", highest)
		return
	}

	blocks, err := p.hiveBlocks.FetchStoredBlocks(startBh, endBh)
	if err != nil {
		prefetchLog.Warn("FetchStoredBlocks failed", "from", startBh, "to", endBh, "err", err)
		return
	}

	queued := 0
	scanned := 0
	produceBlockOps := 0
	for _, blk := range blocks {
		for _, tx := range blk.Transactions {
			for _, op := range tx.Operations {
				scanned++
				if op.Type != "custom_json" {
					continue
				}
				idVal, _ := op.Value["id"].(string)
				if idVal != "vsc.produce_block" {
					continue
				}
				produceBlockOps++
				jsonStr, _ := op.Value["json"].(string)
				if jsonStr == "" {
					continue
				}
				var parsed struct {
					SignedBlock struct {
						Block string `json:"block"`
					} `json:"signed_block"`
				}
				if err := json.Unmarshal([]byte(jsonStr), &parsed); err != nil {
					continue
				}
				blockCidStr := parsed.SignedBlock.Block
				if blockCidStr == "" {
					continue
				}
				if !p.shouldQueue(blockCidStr, blk.BlockNumber) {
					continue
				}
				c, err := cid.Parse(blockCidStr)
				if err != nil {
					continue
				}
				select {
				case p.workQueue <- c:
					queued++
					p.statsMu.Lock()
					p.totalQueued++
					p.statsMu.Unlock()
				case <-p.ctx.Done():
					return
				default:
					// queue full — drop this CID. The next scanOnce will
					// re-queue it (after pruning) once workers catch up.
					p.unmarkSeen(blockCidStr)
				}
			}
		}
	}

	p.pruneSeen(current)

	prefetchLog.Verbose("scan complete",
		"from", startBh,
		"to", endBh,
		"blocks", len(blocks),
		"ops_scanned", scanned,
		"produce_block_ops", produceBlockOps,
		"queued", queued,
		"queue_depth", len(p.workQueue),
	)
}

// shouldQueue marks a CID as seen and returns true if it wasn't already queued.
func (p *BlockPrefetcher) shouldQueue(cidStr string, bh uint64) bool {
	p.seenMu.Lock()
	defer p.seenMu.Unlock()
	if _, ok := p.seen[cidStr]; ok {
		return false
	}
	p.seen[cidStr] = bh
	return true
}

// unmarkSeen removes a CID from the seen set so a later scan can re-queue it.
func (p *BlockPrefetcher) unmarkSeen(cidStr string) {
	p.seenMu.Lock()
	delete(p.seen, cidStr)
	p.seenMu.Unlock()
}

// pruneSeen drops entries for blocks already processed by the state engine,
// keeping the seen set bounded across long reindexes.
func (p *BlockPrefetcher) pruneSeen(currentBh uint64) {
	p.seenMu.Lock()
	defer p.seenMu.Unlock()
	for k, v := range p.seen {
		if v <= currentBh {
			delete(p.seen, k)
		}
	}
}

// fetchWorker drains the work queue and pulls each CID into the IPFS blockstore.
//
// Each fetch runs under a context derived from p.ctx with a per-CID timeout.
// That bounds the cost of an unfetchable CID to prefetchFetchTimeout instead of
// forever, and lets Stop() unblock any in-flight fetch immediately.
func (p *BlockPrefetcher) fetchWorker(id int) {
	defer p.workersWg.Done()
	for c := range p.workQueue {
		fetchStart := time.Now()
		fetchCtx, cancel := context.WithTimeout(p.ctx, prefetchFetchTimeout)
		_, err := p.da.GetDagCtx(fetchCtx, c)
		cancel()
		elapsed := time.Since(fetchStart)
		globalProfile.Record("prefetch.fetch", elapsed)

		timedOut := err != nil && fetchCtx.Err() == context.DeadlineExceeded
		p.statsMu.Lock()
		p.totalFetched++
		p.totalFetchNs += int64(elapsed)
		if err != nil {
			p.totalErrored++
			if timedOut {
				p.totalTimeout++
			}
		}
		p.statsMu.Unlock()

		if err != nil {
			globalProfile.Record("prefetch.fetch_err", elapsed)
			if timedOut {
				globalProfile.Record("prefetch.fetch_timeout", elapsed)
			}
			prefetchLog.Trace("prefetch fetch failed", "worker", id, "cid", c.String(), "timeout", timedOut, "err", err)
		}
	}
}

