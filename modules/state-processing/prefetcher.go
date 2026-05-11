package state_engine

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"vsc-node/lib/datalayer"
	"vsc-node/lib/vsclog"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	"vsc-node/modules/common/common_types"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/hive_blocks"
	vscBlocks "vsc-node/modules/db/vsc/vsc_blocks"

	"github.com/chebyrash/promise"
	"github.com/ipfs/go-cid"
	dagCbor "github.com/ipfs/go-ipld-cbor"
)

var _ aggregate.Plugin = (*BlockPrefetcher)(nil)

var prefetchLog = vsclog.Module("se-prefetch")

// prefetchFetchTimeout caps how long a single GetDag may block. Without it, a
// CID no peer is serving would pin a worker forever — eight bad CIDs would
// drain the worker pool and silently kill prefetching for the rest of the run.
const prefetchFetchTimeout = 60 * time.Second

// stateMerklePrefetchTimeout caps how long EnumerateCids + PrefetchBlocks
// may take for a single state merkle tree. Large trees may need more time.
const stateMerklePrefetchTimeout = 30 * time.Second

// stateMerklePrefetchParallelism is the number of parallel block fetches
// during state merkle tree prefetch.
const stateMerklePrefetchParallelism = 8

// BlockPrefetcher scans upcoming hive blocks for vsc.produce_block ops,
// extracts the referenced VSC block CIDs, and issues concurrent IPFS fetches
// to populate the local blockstore before the state engine processes them.
//
// It also prefetches contract state merkle trees so that DataBin lookups
// during contract execution find all blocks already in local storage.
type BlockPrefetcher struct {
	hiveBlocks    hive_blocks.HiveBlocks
	da            *datalayer.DataLayer
	blockStatus   common_types.BlockStatusGetter
	contractState contracts.ContractState

	lookahead    uint64
	parallelism  int
	scanInterval time.Duration

	workQueue      chan cid.Cid
	txWorkQueue    chan cid.Cid // separate queue for transaction CIDs inside VSC blocks
	stateWorkQueue chan cid.Cid // queue for contract state merkle root CIDs

	seenMu sync.Mutex
	seen   map[string]uint64 // cid string → hive block height it was queued for

	stateSeenMu sync.Mutex
	stateSeen   map[string]bool // state merkle root CID strings already queued

	statsMu           sync.Mutex
	totalQueued       uint64
	totalFetched      uint64
	totalErrored      uint64
	totalTimeout      uint64
	totalFetchNs      int64
	totalTxQueued     uint64
	totalTxFetched    uint64
	totalTxFetchNs    int64
	totalStateQueued  uint64
	totalStateFetched uint64
	totalStateFetchNs int64

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
	contractState contracts.ContractState,
	lookahead uint64,
	parallelism int,
) *BlockPrefetcher {
	return &BlockPrefetcher{
		hiveBlocks:    hiveBlocks,
		da:            da,
		blockStatus:   blockStatus,
		contractState: contractState,
		lookahead:     lookahead,
		parallelism:   parallelism,
		scanInterval:  time.Second,
		seen:          make(map[string]uint64),
		stateSeen:     make(map[string]bool),
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
		p.txWorkQueue = make(chan cid.Cid, p.parallelism*8)    // larger queue for txs
		p.stateWorkQueue = make(chan cid.Cid, p.parallelism*16) // even larger for state merkle

		p.scanWg.Add(1)
		go p.scanLoop()

		for i := 0; i < p.parallelism; i++ {
			p.workersWg.Add(1)
			go p.fetchWorker(i)
		}
		// Transaction prefetch workers
		for i := 0; i < p.parallelism; i++ {
			p.workersWg.Add(1)
			go p.txFetchWorker(i)
		}
		// State merkle prefetch workers
		for i := 0; i < p.parallelism; i++ {
			p.workersWg.Add(1)
			go p.stateMerkleWorker(i)
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
			txQueued := p.totalTxQueued
			txFetched := p.totalTxFetched
			txNs := p.totalTxFetchNs
			stateQueued := p.totalStateQueued
			stateFetched := p.totalStateFetched
			stateNs := p.totalStateFetchNs
			p.statsMu.Unlock()
			prefetchQueueDepth.Set(float64(len(p.workQueue)))
			var avgMs int64
			if fetched > 0 {
				avgMs = (ns / int64(fetched)) / int64(time.Millisecond)
			}
			var txAvgMs int64
			if txFetched > 0 {
				txAvgMs = (txNs / int64(txFetched)) / int64(time.Millisecond)
			}
			var stateAvgMs int64
			if stateFetched > 0 {
				stateAvgMs = (stateNs / int64(stateFetched)) / int64(time.Millisecond)
			}
			prefetchLog.Debug("prefetch stats",
				"queued", queued,
				"fetched", fetched,
				"errored", errored,
				"timeout", timedOut,
				"avg_fetch_ms", avgMs,
				"queue_depth", len(p.workQueue),
				"tx_queued", txQueued,
				"tx_fetched", txFetched,
				"tx_avg_fetch_ms", txAvgMs,
				"tx_queue_depth", len(p.txWorkQueue),
				"state_queued", stateQueued,
				"state_fetched", stateFetched,
				"state_avg_fetch_ms", stateAvgMs,
				"state_queue_depth", len(p.stateWorkQueue),
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
	close(p.txWorkQueue)
	close(p.stateWorkQueue)
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
					prefetchQueued.Inc()
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
func (p *BlockPrefetcher) fetchWorker(id int) {
	defer p.workersWg.Done()
	for c := range p.workQueue {
		fetchStart := time.Now()
		fetchCtx, cancel := context.WithTimeout(p.ctx, prefetchFetchTimeout)
		node, err := p.da.GetDagCtx(fetchCtx, c)
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
				prefetchFetchErrors.WithLabelValues("timeout").Inc()
			} else {
				prefetchFetchErrors.WithLabelValues("error").Inc()
			}
			prefetchLog.Trace("prefetch fetch failed", "worker", id, "cid", c.String(), "timeout", timedOut, "err", err)
			continue
		}

		// Try to extract transaction CIDs from VSC block
		if node != nil {
			p.extractTxCids(node, c)
		}
	}
}

// extractTxCids attempts to parse a VSC block and queue its transaction CIDs
// for prefetching. This is best-effort - if parsing fails, we just skip it.
// Also extracts contract calls from VSC block transactions to queue state
// merkle tree CIDs for prefetch.
func (p *BlockPrefetcher) extractTxCids(node *dagCbor.Node, blockCid cid.Cid) {
	var block vscBlocks.VscBlock
	if err := dagCbor.DecodeInto(node.RawData(), &block); err != nil {
		// Not a VSC block (likely a transaction or other CID type)
		return
	}

	// Successfully parsed as VSC block - extract transaction CIDs
	txQueued := 0
	for _, tx := range block.Transactions {
		if tx.Id == "" {
			continue
		}
		txCid, err := cid.Parse(tx.Id)
		if err != nil {
			continue
		}
		select {
		case p.txWorkQueue <- txCid:
			txQueued++
		default:
			// Transaction queue full - drop this CID
			break
		}
	}

	if txQueued > 0 {
		p.statsMu.Lock()
		p.totalTxQueued += uint64(txQueued)
		p.statsMu.Unlock()
	}
}

// txFetchWorker drains the transaction work queue and prefetches transaction CIDs.
// It also attempts to extract contract call info from each transaction to queue
// state merkle tree CIDs for prefetching.
func (p *BlockPrefetcher) txFetchWorker(id int) {
	defer p.workersWg.Done()
	for c := range p.txWorkQueue {
		fetchStart := time.Now()
		fetchCtx, cancel := context.WithTimeout(p.ctx, prefetchFetchTimeout)
		node, err := p.da.GetDagCtx(fetchCtx, c)
		cancel()
		elapsed := time.Since(fetchStart)
		globalProfile.Record("prefetch.tx_fetch", elapsed)

		p.statsMu.Lock()
		p.totalTxFetched++
		p.totalTxFetchNs += int64(elapsed)
		p.statsMu.Unlock()

		if err != nil {
			prefetchLog.Trace("prefetch tx failed", "worker", id, "cid", c.String(), "err", err)
			continue
		}

		// Try to extract contract calls and queue state merkle CIDs
		if node != nil {
			p.extractContractStateCids(node.RawData())
		}
	}
}

// extractContractStateCids attempts to decode a VSC transaction, find contract
// calls, look up the contracts' state merkle CIDs, and queue them for prefetch.
func (p *BlockPrefetcher) extractContractStateCids(rawData []byte) {
	// VSC transactions use EncodeDagCbor (struct→JSON→IPLD→CBOR), so the
	// CBOR map keys match the json tags, not refmt tags or Go field names.
	// Use common.DecodeCbor which goes through the same JSON intermediate path
	// (CBOR→cbornode→JSON→struct) and therefore matches correctly.
	var shell struct {
		Tx []struct {
			Type    string `json:"type"`
			Payload []byte `json:"payload"`
		} `json:"tx"`
	}
	if err := common.DecodeCbor(rawData, &shell); err != nil {
		return
	}

	for _, op := range shell.Tx {
		if op.Type != "call" {
			continue
		}
		// Decode the call payload (CBOR-encoded TxVscCallContract).
		var call struct {
			ContractId string `json:"contract_id"`
		}
		if err := common.DecodeCbor(op.Payload, &call); err != nil {
			continue
		}
		if call.ContractId == "" {
			continue
		}

		// Deduplicate by contract ID within the batch using stateSeen.
		if !p.markStateSeen(call.ContractId) {
			continue
		}

		// Look up the contract's state merkle CID from the database.
		output, err := p.contractState.GetLastOutput(call.ContractId, p.blockStatus.BlockHeight())
		if err != nil || output.StateMerkle == "" {
			continue
		}
		root, err := cid.Parse(output.StateMerkle)
		if err != nil {
			continue
		}

		// Queue the state merkle root CID for prefetch.
		select {
		case p.stateWorkQueue <- root:
			p.statsMu.Lock()
			p.totalStateQueued++
			p.statsMu.Unlock()
		default:
			// queue full — drop
		}
	}
}

// markStateSeen returns true if the contract ID wasn't already seen in this
// prefetch window, and marks it as seen.
func (p *BlockPrefetcher) markStateSeen(contractId string) bool {
	p.stateSeenMu.Lock()
	defer p.stateSeenMu.Unlock()
	if p.stateSeen[contractId] {
		return false
	}
	p.stateSeen[contractId] = true
	return true
}

// stateMerkleWorker drains the state work queue and prefetches all blocks in
// each contract's state merkle tree into the local blockstore.
func (p *BlockPrefetcher) stateMerkleWorker(id int) {
	defer p.workersWg.Done()
	for root := range p.stateWorkQueue {
		fetchStart := time.Now()

		// Enumerate all CIDs in the state merkle tree, then prefetch them.
		ctx, cancel := context.WithTimeout(p.ctx, stateMerklePrefetchTimeout)
		cids, err := p.da.EnumerateCids(ctx, root)
		if err != nil {
			cancel()
			prefetchLog.Trace("state merkle enumerate failed", "worker", id, "root", root.String(), "err", err)
			continue
		}
		// EnumerateCids already fetched the tree structure via DagServ.Get;
		// PrefetchBlocks ensures any remaining leaf blocks are local too.
		_ = p.da.PrefetchBlocks(ctx, cids, stateMerklePrefetchParallelism)
		cancel()

		elapsed := time.Since(fetchStart)
		globalProfile.Record("prefetch.state_merkle", elapsed)

		p.statsMu.Lock()
		p.totalStateFetched++
		p.totalStateFetchNs += int64(elapsed)
		p.statsMu.Unlock()
	}
}
