package datalayer

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/chebyrash/promise"
	"github.com/libp2p/go-libp2p/core/peer"

	bitswap "github.com/ipfs/boxo/bitswap"
	"github.com/ipfs/boxo/bitswap/network"
	"github.com/ipfs/boxo/blockservice"
	blockstore "github.com/ipfs/boxo/blockstore"
	"github.com/ipfs/boxo/datastore/dshelp"
	"github.com/ipfs/boxo/exchange/providing"
	"github.com/ipfs/boxo/ipld/merkledag"
	"github.com/ipfs/boxo/provider"
	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	badger "github.com/ipfs/go-ds-badger2"
	cbornode "github.com/ipfs/go-ipld-cbor"
	format "github.com/ipfs/go-ipld-format"

	"vsc-node/lib/utils"
	"vsc-node/lib/vsclog"
	"vsc-node/modules/common/common_types"
	libp2p "vsc-node/modules/p2p"

	goJson "encoding/json"

	dagCbor "github.com/ipfs/go-ipld-cbor"
	"github.com/multiformats/go-multicodec"
	mh "github.com/multiformats/go-multihash"

	a "vsc-node/modules/aggregate"
)

const dagFetchTimeout = 60 * time.Second
const dagFetchMaxRetries = 3

// failedCidTTL controls how long a CID stays in the negative cache before
// we allow another retry cycle. Tuned for transient peer-routing hiccups —
// long enough to avoid burning minutes on a hopelessly-missing CID, short
// enough that legitimately healed network state recovers quickly.
const failedCidTTL = 5 * time.Minute

// negCacheMaxEntries caps the negative cache so a flood of unreachable CIDs
// can't grow the map without bound. Past this size, expired entries are
// purged eagerly; if everything is still within TTL, the oldest are
// evicted regardless.
const negCacheMaxEntries = 4096

var daLog = vsclog.Module("datalayer")

// timeoutBlockService wraps a BlockService so that every GetBlock call —
// including implicit ones from DagServ / merkledag traversal — gets a
// per-call timeout. This bounds blocking on HAMT shard reads,
// NewDataBinFromCid traversal, and any other implicit consumer so a
// single unreachable CID can't stall a fetch indefinitely.
//
// Note: this layer does NOT retry. Retries live in DataLayer.getBlockBounded
// so they only apply at explicitly-tagged Bounded / Skippable call sites,
// not to incidental traversal which would otherwise compound the failure
// cost (e.g. one missing HAMT shard becoming 4 × 60s = 4 minutes of stall
// for what was previously a fast-fail).
type timeoutBlockService struct {
	blockservice.BlockService
}

func (t *timeoutBlockService) GetBlock(ctx context.Context, c cid.Cid) (blocks.Block, error) {
	tCtx, cancel := context.WithTimeout(ctx, dagFetchTimeout)
	defer cancel()
	return t.BlockService.GetBlock(tCtx, c)
}

// negCache is a per-DataLayer negative cache used only by Skippable fetches.
// Bounded fetches do not consult it — every call retries fresh.
type negCache struct {
	mu sync.RWMutex
	m  map[cid.Cid]time.Time
}

func newNegCache() *negCache {
	return &negCache{m: make(map[cid.Cid]time.Time)}
}

func (n *negCache) recentlyFailed(c cid.Cid) (time.Duration, bool) {
	n.mu.RLock()
	t, ok := n.m[c]
	n.mu.RUnlock()
	if !ok {
		return 0, false
	}
	age := time.Since(t)
	if age < failedCidTTL {
		return age, true
	}
	// Lazy eviction: TTL elapsed, drop the entry so the map can shrink
	// without waiting for a periodic GC.
	n.mu.Lock()
	if t2, ok := n.m[c]; ok && time.Since(t2) >= failedCidTTL {
		delete(n.m, c)
	}
	n.mu.Unlock()
	return 0, false
}

func (n *negCache) markFailed(c cid.Cid) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.m[c] = time.Now()
	if len(n.m) > negCacheMaxEntries {
		n.evictLocked()
	}
}

func (n *negCache) clear(c cid.Cid) {
	n.mu.Lock()
	delete(n.m, c)
	n.mu.Unlock()
}

// evictLocked drops expired entries first; if the map is still over cap,
// drops the oldest entries until it fits. Caller must hold n.mu.
func (n *negCache) evictLocked() {
	cutoff := time.Now().Add(-failedCidTTL)
	for k, t := range n.m {
		if t.Before(cutoff) {
			delete(n.m, k)
		}
	}
	if len(n.m) <= negCacheMaxEntries {
		return
	}
	// Degenerate path: every entry is fresh. Find the oldest few and drop
	// them so subsequent failures aren't lost. We only need to drop a
	// handful (one extra per markFailed once we're at cap), so a linear
	// scan beats sorting.
	for len(n.m) > negCacheMaxEntries {
		var oldestK cid.Cid
		var oldestT time.Time
		first := true
		for k, t := range n.m {
			if first || t.Before(oldestT) {
				oldestK = k
				oldestT = t
				first = false
			}
		}
		delete(n.m, oldestK)
	}
}

type DataLayer struct {
	// a.Plugin
	p2pService *libp2p.P2PServer
	bitswap    *bitswap.Bitswap
	blockServ  blockservice.BlockService
	DagServ    format.DAGService
	Datastore  *badger.Datastore

	// negCache is consulted only by *Skippable fetches — Bounded fetches
	// always retry fresh.
	negCache *negCache

	dataDir []string
}

type MetricsCtx context.Context

type GetOptions struct {
	NoStore bool
}

type PutRawOptions struct {
	Codec multicodec.Code
	Pin   bool
}

type PutOptions struct {
	Broadcast bool
}

func (dl *DataLayer) Init() error {
	ctx := context.Background()

	var path string

	if len(dl.dataDir) > 0 && dl.dataDir[0] != "" {
		path = fmt.Sprint(dl.dataDir[0], "/badger")
	} else {
		path = "data/badger"
	}

	ds, err := badger.NewDatastore(path, &badger.DefaultOptions)

	if err != nil {
		panic(err)
	}

	dl.Datastore = ds

	var bstore blockstore.Blockstore = blockstore.NewBlockstore(ds)

	bswapnet := network.NewFromIpfsHost(dl.p2pService)

	// Create Bitswap: a new "discovery" parameter, usually the "contentRouter"
	// which does both discovery and providing.
	bswap := bitswap.New(ctx, bswapnet, dl.p2pService, bstore)
	// A provider system that handles concurrent provides etc. "contentProvider"
	// is usually the "contentRouter" which does both discovery and providing.
	// "contentProvider" could be used directly without wrapping, but it is recommended
	// to do so to provide more efficiently.
	provider, err := provider.New(ds, provider.Online(dl.p2pService))
	if err != nil {
		panic(err)
	}

	// A wrapped providing exchange using the previous exchange and the provider.
	exchange := providing.New(bswap, provider)

	// Wrap the blockservice with a per-call timeout so EVERY GetBlock call
	// — including implicit ones via DagServ / merkledag / HAMT shard
	// traversal — is bounded. Without the wrapper, a single unreachable
	// CID inside a HAMT walk would block indefinitely on bitswap.
	//
	// Retries are deliberately NOT applied here. They live in
	// getBlockBounded / getBlockSkippable so they only fire at
	// explicitly-tagged divergence-critical call sites. Applying retry
	// globally would turn an incidental missing CID encountered during
	// HAMT traversal into a 4 × 60s = 4-minute stall, instead of the
	// fast-fail it should be. The negative cache is also applied only
	// at Skippable sites — implicit consumers see neither retry nor cache.
	blockService := &timeoutBlockService{
		BlockService: blockservice.New(bstore, exchange),
	}
	dl.blockServ = blockService
	dl.bitswap = bswap
	dl.negCache = newNegCache()

	dl.DagServ = merkledag.NewDAGService(blockService)

	return nil
}

func (dl *DataLayer) Start() *promise.Promise[any] {
	return utils.PromiseResolve[any](nil)
}

func (dl *DataLayer) Stop() error {
	if dl.Datastore != nil {
		return dl.Datastore.Close()
	}
	return nil
}

// Will always hash using sha256
func (dl *DataLayer) PutRaw(rawData []byte, options common_types.PutRawOptions) (*cid.Cid, error) {

	prefix := cid.Prefix{
		Version:  1,
		Codec:    uint64(multicodec.Raw),
		MhType:   mh.SHA2_256,
		MhLength: -1,
	}
	cid, _ := prefix.Sum(rawData)

	blockData, _ := blocks.NewBlockWithCid(rawData, cid)

	dl.blockServ.AddBlock(context.TODO(), blockData)

	if options.Pin {
		//Do pin here
		// dl.AddPin(cid, struct{recursive bool}{ recursive: true})
	}

	return &cid, nil
}

func (dl *DataLayer) PutObject(data interface{}, options ...common_types.PutOptions) (*cid.Cid, error) {
	ctx := context.Background()
	cborBytes, err := cbornode.Encode(data)

	if err != nil {
		return nil, err
	}

	prefix := cid.Prefix{
		Version:  1,
		Codec:    uint64(multicodec.DagCbor),
		MhType:   mh.SHA2_256,
		MhLength: -1,
	}
	cid, err := prefix.Sum(cborBytes)

	if err != nil {
		return nil, err
	}

	blockData, _ := blocks.NewBlockWithCid(cborBytes, cid)

	dl.blockServ.AddBlock(context.TODO(), blockData)

	// block := blocks.NewBlock(bytes)

	brcst := false
	if len(options) > 0 {
		if options[0].Broadcast {
			brcst = true
		}
	}

	if brcst {
		dl.notify(ctx, blockData)
	} else {
		go dl.notify(ctx, blockData)
	}

	return &cid, nil
}

func (dl *DataLayer) PutJson(data interface{}, options ...common_types.PutOptions) (*cid.Cid, error) {
	jsonBytes, err := goJson.Marshal(data)

	if err != nil {
		return nil, err
	}

	dagNode, err := cbornode.FromJSON(bytes.NewReader(jsonBytes), mh.SHA2_256, -1)

	if err != nil {
		return nil, err
	}

	err = dl.blockServ.AddBlock(context.Background(), dagNode)

	if err != nil {
		return nil, err
	}

	ccid := dagNode.Cid()

	return &ccid, nil
}

func (dl *DataLayer) HashObject(data interface{}) (*cid.Cid, error) {
	cborBytes, err := cbornode.Encode(data)

	if err != nil {
		return nil, err
	}

	prefix := cid.Prefix{
		Version:  1,
		Codec:    uint64(multicodec.DagCbor),
		MhType:   mh.SHA2_256,
		MhLength: -1,
	}
	cid, err := prefix.Sum(cborBytes)

	return &cid, err
}

// fetchWithRetry runs the multi-attempt retry loop against the wrapped
// BlockService (which already applies a per-call timeout). Used by both
// Bounded and Skippable helpers; NOT used by implicit DagServ traversal,
// which gets only the per-call timeout.
func (dl *DataLayer) fetchWithRetry(c cid.Cid) (blocks.Block, error) {
	ctx := context.Background()
	var lastErr error
	backoff := 5 * time.Second

	for attempt := 0; attempt <= dagFetchMaxRetries; attempt++ {
		if attempt > 0 {
			daLog.Warn("retrying block fetch", "cid", c, "attempt", attempt, "backoff", backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			backoff *= 2
		}

		block, err := dl.blockServ.GetBlock(ctx, c)
		if err == nil {
			return block, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("fetch(%s): all %d attempts failed: %w", c, dagFetchMaxRetries+1, lastErr)
}

// getBlockBounded performs a fetch with timeout + bounded retries. It
// NEVER consults or writes the negative cache, so transient misses don't
// poison subsequent access. Use from sites where a missed CID risks state
// divergence (oplog ingestion, election ingestion, top-level block DAG
// fetches) — the caller is responsible for treating the returned error
// as a halt signal.
func (dl *DataLayer) getBlockBounded(c cid.Cid) (blocks.Block, error) {
	return dl.fetchWithRetry(c)
}

// getBlockSkippable wraps fetchWithRetry with a negative cache: after the
// retry cycle exhausts, the CID fast-fails for `failedCidTTL` instead of
// paying the retry cost again. Use ONLY at sites where skipping the CID
// is structurally healed by the protocol (e.g. contract output ingestion,
// where the next ContractOutput's upsert overwrites the `state_merkle`
// pointer regardless of whether the missed one was applied).
func (dl *DataLayer) getBlockSkippable(c cid.Cid) (blocks.Block, error) {
	if age, recent := dl.negCache.recentlyFailed(c); recent {
		return nil, fmt.Errorf("GetBlock(%s): skipped (unreachable, last failed %s ago)", c, age.Round(time.Second))
	}

	block, err := dl.fetchWithRetry(c)
	if err != nil {
		dl.negCache.markFailed(c)
		daLog.Warn("block unreachable, adding to negative cache",
			"cid", c, "ttl", failedCidTTL, "err", err)
		return nil, err
	}
	dl.negCache.clear(c)
	return block, nil
}

// Get fetches and decodes a CBOR-encoded node, with bounded retry semantics.
// See getBlockBounded — failures here propagate up; callers that want
// skip-on-miss should use GetSkippable instead.
func (dl *DataLayer) Get(cid cid.Cid, options *common_types.GetOptions) (format.Node, error) {
	//This is using direct bitswap access which may not use a block store.
	//Thus, it will not store anything upon request.
	block, err := dl.getBlockBounded(cid)
	if err != nil {
		return nil, err
	}
	node, err := dagCbor.DecodeBlock(block)

	dl.blockServ.AddBlock(context.Background(), block)

	if err != nil {
		return nil, err
	}
	return node, nil
}

// GetSkippable is the negative-cached counterpart to Get — only safe at sites
// where skipping the CID is structurally healed by the protocol.
func (dl *DataLayer) GetSkippable(cid cid.Cid) (format.Node, error) {
	block, err := dl.getBlockSkippable(cid)
	if err != nil {
		return nil, err
	}
	node, err := dagCbor.DecodeBlock(block)

	dl.blockServ.AddBlock(context.Background(), block)

	if err != nil {
		return nil, err
	}
	return node, nil
}

// Gets Object then converts it to Golang type seemlessly
func (dl *DataLayer) GetObject(cid cid.Cid, v interface{}, options common_types.GetOptions) error {
	dataNode, err := dl.Get(cid, &options)

	if err != nil {
		return err
	}

	err = cbornode.DecodeInto(dataNode.RawData(), v)

	return err
}

func (dl *DataLayer) GetDag(cid cid.Cid) (*dagCbor.Node, error) {
	block, err := dl.getBlockBounded(cid)
	if err != nil {
		return nil, err
	}
	dag, err := dagCbor.Decode(block.RawData(), mh.SHA2_256, -1)
	return dag, err
}

// GetDagSkippable is the negative-cached counterpart to GetDag — only safe
// at sites where skipping the CID is structurally healed by the protocol.
func (dl *DataLayer) GetDagSkippable(cid cid.Cid) (*dagCbor.Node, error) {
	block, err := dl.getBlockSkippable(cid)
	if err != nil {
		return nil, err
	}
	return dagCbor.Decode(block.RawData(), mh.SHA2_256, -1)
}

func (dl *DataLayer) GetRaw(cid cid.Cid) ([]byte, error) {
	block, err := dl.getBlockBounded(cid)
	if err != nil {
		return nil, err
	}
	return block.RawData(), nil
}

func (dl *DataLayer) notify(ctx context.Context, block blocks.Block) {
	dl.bitswap.NotifyNewBlocks(ctx, block)
	//We might need to proactively rebroadcast that we are storing a CID
	dl.p2pService.BroadcastCidWithContext(ctx, block.Cid())
}

type AddPin struct {
}

type pinRecord struct {
	Type string
}

func (dl *DataLayer) AddPin(pinCid cid.Cid, options struct {
	recursive bool
}) {
	var Type string

	if options.recursive {
		//Do recursive pinning of all child nodes
		Type = "recursive"
	} else {
		Type = "direct"
	}

	blkExists, _ := dl.Datastore.Has(context.Background(), dshelp.MultihashToDsKey(pinCid.Hash()))

	if !blkExists {
		dl.blockServ.GetBlock(context.Background(), pinCid)
	}

	pnr := pinRecord{
		Type: Type,
	}
	jsonPnr, _ := goJson.Marshal(pnr)
	dl.Datastore.Put(context.TODO(), datastore.NewKey("/pins/"+pinCid.String()), jsonPnr)
}

func (dl *DataLayer) RmPin(pinCid cid.Cid, options struct {
}) {
	key := datastore.NewKey("/pins/" + pinCid.String())
	pinExists, _ := dl.Datastore.Has(context.Background(), key)

	//Delete from pin list
	//Actual content removal will take place when GC happens
	if pinExists {
		dl.Datastore.Delete(context.Background(), key)
	}
}

func (dl *DataLayer) FindProviders(cid.Cid) []peer.ID {

	return make([]peer.ID, 0)
}

var _ a.Plugin = &DataLayer{}

func New(p2pService *libp2p.P2PServer, dataDir ...string) *DataLayer {

	return &DataLayer{

		p2pService: p2pService,
		dataDir:    dataDir,
	}
}
