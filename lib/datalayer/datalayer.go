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
	"vsc-node/modules/common/common_types"
	libp2p "vsc-node/modules/p2p"

	goJson "encoding/json"

	dagCbor "github.com/ipfs/go-ipld-cbor"
	"github.com/multiformats/go-multicodec"
	mh "github.com/multiformats/go-multihash"

	a "vsc-node/modules/aggregate"
)

type DataLayer struct {
	// a.Plugin
	p2pService *libp2p.P2PServer
	bitswap    *bitswap.Bitswap
	blockServ  blockservice.BlockService
	DagServ    format.DAGService
	Datastore  *badger.Datastore

	dataDir []string
}

// Bitswap exposes the underlying bitswap instance for diagnostics/probe use.
func (dl *DataLayer) Bitswap() *bitswap.Bitswap { return dl.bitswap }

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

	// Finally the blockservice
	blockService := blockservice.New(bstore, exchange)
	dl.blockServ = blockService
	dl.bitswap = bswap

	dl.DagServ = merkledag.NewDAGService(blockService)

	// Pre-populate the universal empty-bytes block. boxo's bitswap server has
	// a bug where getBlockSizes filters zero-size results out of its return
	// map, causing the engine to respond DontHave for any 0-byte block even
	// when blockstore.Has returns true (boxo@v0.27.2 — and current main —
	// blockstoremanager.go: `if n != 0 { res[ks[i]] = n }`). Contracts that
	// store empty values produce the universal CID
	// bafkreihdwdcefgh4dqkjv67uzcmw7ojee6xedzdetojuzjevtenxquvyku, and any
	// node lacking it locally would hang forever in the unbounded GetDag
	// path during state reads. Writing the block here makes blockservice.GetBlock
	// short-circuit on the local-blockstore lookup before it ever falls through
	// to bitswap, so every node self-heals at startup.
	emptyPrefix := cid.Prefix{Version: 1, Codec: uint64(multicodec.Raw), MhType: mh.SHA2_256, MhLength: -1}
	emptyCid, _ := emptyPrefix.Sum(nil)
	emptyBlock, _ := blocks.NewBlockWithCid(nil, emptyCid)
	if err := blockService.AddBlock(ctx, emptyBlock); err != nil {
		return fmt.Errorf("failed to seed empty-bytes block: %w", err)
	}

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

func (dl *DataLayer) Get(cid cid.Cid, options *common_types.GetOptions) (format.Node, error) {
	//This is using direct bitswap access which may not use a block store.
	//Thus, it will not store anything upon request.
	block, err := dl.blockServ.GetBlock(context.Background(), cid)
	if err != nil {
		return nil, err
	}
	node, err := dagCbor.DecodeBlock(block)

	dl.blockServ.AddBlock(context.Background(), block)

	if err != nil {
		return nil, err
	}
	return node, nil
	// if options.NoStore {
	// } else {
	// 	//This will automatically store locally
	// 	node, err := dl.DagServ.Get(context.Background(), cid)
	// 	return &node, err
	// }
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
	return dl.GetDagCtx(context.Background(), cid)
}

// GetDagCtx is GetDag with a caller-supplied context. Use this when the fetch
// must be cancellable (e.g. a per-request timeout) so a bad or unfetchable CID
// can't hang the goroutine forever.
func (dl *DataLayer) GetDagCtx(ctx context.Context, c cid.Cid) (*dagCbor.Node, error) {
	block, err := dl.blockServ.GetBlock(ctx, c)
	if err != nil {
		return nil, err
	}
	return dagCbor.Decode(block.RawData(), mh.SHA2_256, -1)
}

// GetRawCtx fetches the raw block data for the given CID, using the
// caller-supplied context for cancellation/timeout control.
func (dl *DataLayer) GetRawCtx(ctx context.Context, cid cid.Cid) ([]byte, error) {
	block, err := dl.blockServ.GetBlock(ctx, cid)
	if err != nil {
		return nil, err
	}
	return block.RawData(), nil
}

func (dl *DataLayer) GetRaw(cid cid.Cid) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return dl.GetRawCtx(ctx, cid)
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

// EnumerateCids walks the DAG rooted at `root` and returns every CID in the
// graph, including the root itself, all internal directory/shard nodes, and
// all leaf data blocks. The returned slice is in no particular order.
// This is used to discover the full set of blocks that must be prefetched
// to bring a contract state merkle tree entirely into local storage.
func (dl *DataLayer) EnumerateCids(ctx context.Context, root cid.Cid) ([]cid.Cid, error) {
	seen := make(map[string]bool)
	var cids []cid.Cid
	if err := dl.enumerateCidsRecursive(ctx, root, seen, &cids); err != nil {
		return nil, err
	}
	return cids, nil
}

func (dl *DataLayer) enumerateCidsRecursive(ctx context.Context, c cid.Cid, seen map[string]bool, cids *[]cid.Cid) error {
	key := c.String()
	if seen[key] {
		return nil
	}
	seen[key] = true
	*cids = append(*cids, c)

	node, err := dl.DagServ.Get(ctx, c)
	if err != nil {
		return err
	}
	for _, link := range node.Links() {
		if err := dl.enumerateCidsRecursive(ctx, link.Cid, seen, cids); err != nil {
			return err
		}
	}
	return nil
}

// PrefetchBlocks fetches a batch of CIDs into the local blockstore using the
// given parallelism. Already-local blocks return immediately (blockstore hit);
// remote blocks are fetched via bitswap. Callers should use a context with a
// reasonable timeout to bound the effect of unfetchable CIDs.
func (dl *DataLayer) PrefetchBlocks(ctx context.Context, cids []cid.Cid, parallelism int) error {
	if len(cids) == 0 || parallelism <= 0 {
		return nil
	}

	sem := make(chan struct{}, parallelism)
	var wg sync.WaitGroup
	var errsMu sync.Mutex
	var errs []error

	for _, c := range cids {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		wg.Add(1)
		go func(c cid.Cid) {
			defer wg.Done()
			defer func() { <-sem }()

			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}

			select {
			case <-ctx.Done():
				return
			default:
			}

			// Use GetBlock to pull into local blockstore. If already present
			// it returns immediately from Badger; otherwise bitswap fetches it.
			if _, err := dl.blockServ.GetBlock(ctx, c); err != nil {
				errsMu.Lock()
				errs = append(errs, err)
				errsMu.Unlock()
			}
		}(c)
	}
	wg.Wait()

	if len(errs) > 0 {
		return fmt.Errorf("prefetch: %d/%d blocks failed (first: %w)", len(errs), len(cids), errs[0])
	}
	return nil
}

var _ a.Plugin = &DataLayer{}

func New(p2pService *libp2p.P2PServer, dataDir ...string) *DataLayer {

	return &DataLayer{

		p2pService: p2pService,
		dataDir:    dataDir,
	}
}
