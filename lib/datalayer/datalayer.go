package datalayer

import (
	"bytes"
	"context"
	"fmt"
	"sync/atomic"

	"github.com/chebyrash/promise"
	"github.com/libp2p/go-libp2p/core/peer"

	bitswap "github.com/ipfs/boxo/bitswap"
	"github.com/ipfs/boxo/bitswap/network"
	"github.com/ipfs/boxo/blockservice"
	blockstore "github.com/ipfs/boxo/blockstore"
	"github.com/ipfs/boxo/datastore/dshelp"
	"github.com/ipfs/boxo/exchange/offline"
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
	// providerStore holds the provider's queue of CIDs to announce, apart
	// from the block store (see Init); provSys and asyncProv are the provider
	// and the hand-off in front of it, closed by Stop.
	providerStore *badger.Datastore
	provSys       provider.System
	asyncProv     *asyncProvider

	dataDir []string

	// local is a read view over the same blockstore that never asks the network.
	// Built in Init; see LocalOnly.
	local *DataLayer
	// localOnly is set on that view.
	localOnly bool
	// missed, on a view from LocalOnlyTracking, records that a state read
	// failed and was answered as "empty" or "absent". nil on every other view.
	missed *atomic.Bool
}

// LocalOnlyTracking returns a NEW local-only view (the same local blockstore,
// never the network) that remembers whether a state read through it failed:
// Missed. It is for one GraphQL request.
//
// A local-only read of contract state that fails (a block of the state is not
// in the local store at that moment) is answered as an empty directory or an
// absent key, so that an API caller cannot park the node on the network. The
// caller cannot tell that from state that really is empty: LOCALONLY-EMPTY-1
// (testnet 11-16/17: getStateByKeys answered null for keys that exist, and dry
// runs ran on empty contract state, for up to about a second after a block
// changed that state). With this view the request can see that it happened and
// read again or say so.
//
// The shared view from LocalOnly is not changed and records nothing: the TSS
// and oracle readers that use it behave as before.
func (dl *DataLayer) LocalOnlyTracking() *DataLayer {
	if dl == nil || dl.LocalOnly() == nil {
		return nil // before Init, as LocalOnly
	}
	base := dl.LocalOnly()
	v := *base
	v.missed = &atomic.Bool{}
	v.local = &v
	return &v
}

// NewOffline returns a data layer over dag that never uses a network, with its
// local-only views in place: for tests and tools that read state from a DAG
// they hold.
func NewOffline(dag format.DAGService) *DataLayer {
	dl := &DataLayer{DagServ: dag}
	dl.local = &DataLayer{DagServ: dag, localOnly: true}
	dl.local.local = dl.local
	return dl
}

// Missed reports whether a state read through this view failed and was
// answered as empty or absent. Always false on a view that does not track.
func (dl *DataLayer) Missed() bool {
	return dl != nil && dl.missed != nil && dl.missed.Load()
}

// noteMiss records a failed local-only state read on a tracking view.
func (dl *DataLayer) noteMiss() {
	if dl.missed != nil {
		dl.missed.Store(true)
	}
}

// LocalOnly returns a view of the data layer that reads only blocks already in
// the local blockstore and never fetches from the network. It is for the GraphQL
// API, whose callers choose the CIDs: through the full data layer, a CID the
// node does not have makes bitswap search the network with no deadline, parking
// the request goroutine, and anything it does fetch is stored with no garbage
// collection, so any client could pile up hung requests and grow the disk. A
// synced node already holds everything the API serves (blocks, outputs, contract
// code and state, elections), because applying those blocks stored it locally.
// Nil before Init.
func (dl *DataLayer) LocalOnly() *DataLayer {
	return dl.local
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
	// The provider announces every block this node stores, through a queue it
	// keeps in a datastore: one write when a CID is queued and one when it is
	// announced, each its own transaction, and the block store's datastore
	// flushes every transaction to disk. Every block write waited on that queue
	// (its Enqueue hands over to one worker), so a slot that stored many blocks
	// was slow in proportion to their number. The queue gets its own store,
	// written without a flush per entry. A crash can lose the CIDs still queued;
	// this node does not announce those again (other holders announce their
	// own), and announcements never decide state.
	provDs, err := openProviderStore(path + "-provider")
	if err != nil {
		panic(err)
	}
	dl.providerStore = provDs
	provSys, err := provider.New(provDs, provider.Online(dl.p2pService))
	if err != nil {
		panic(err)
	}
	dl.provSys = provSys
	// The provider kept its queue in the block store before; nothing reads
	// those entries any more.
	go dropOldProviderKeys(ds)

	// A wrapped providing exchange using the previous exchange and the provider.
	// Announcing never holds up a block write (asyncProvider).
	dl.asyncProv = newAsyncProvider(provSys)
	exchange := providing.New(bswap, dl.asyncProv)

	// Finally the blockservice
	blockService := blockservice.New(bstore, exchange)
	dl.blockServ = blockService
	dl.bitswap = bswap

	dl.DagServ = merkledag.NewDAGService(blockService)

	localServ := blockservice.New(bstore, offline.Exchange(bstore))
	dl.local = &DataLayer{
		p2pService: dl.p2pService,
		blockServ:  localServ,
		DagServ:    merkledag.NewDAGService(localServ),
		Datastore:  ds,
		dataDir:    dl.dataDir,
		localOnly:  true,
	}
	dl.local.local = dl.local

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
	if dl.asyncProv != nil {
		dl.asyncProv.Close()
	}
	if dl.provSys != nil {
		dl.provSys.Close()
	}
	if dl.providerStore != nil {
		dl.providerStore.Close()
	}
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

// PutRawMany stores raw blocks in one write and returns their CIDs, in order:
// the same CIDs and the same stored blocks as one PutRaw each. (PutRaw names
// every block with the raw codec; the block store keys blocks by multihash, so
// a block is found under any codec's CID for its bytes.)
func (dl *DataLayer) PutRawMany(values [][]byte) ([]cid.Cid, error) {
	prefix := cid.Prefix{Version: 1, Codec: uint64(multicodec.Raw), MhType: mh.SHA2_256, MhLength: -1}
	cids := make([]cid.Cid, len(values))
	blks := make([]blocks.Block, 0, len(values))
	for i, v := range values {
		c, err := prefix.Sum(v)
		if err != nil {
			return nil, err
		}
		b, err := blocks.NewBlockWithCid(v, c)
		if err != nil {
			return nil, err
		}
		cids[i] = c
		blks = append(blks, b)
	}
	if len(blks) == 0 {
		return cids, nil
	}
	if err := dl.blockServ.AddBlocks(context.TODO(), blks); err != nil {
		// The batch is not atomic: some blocks may be stored. Store them one
		// at a time, as PutRaw did, so one failing block loses only itself.
		var firstErr error
		for _, b := range blks {
			if e := dl.blockServ.AddBlock(context.TODO(), b); e != nil && firstErr == nil {
				firstErr = e
			}
		}
		if firstErr != nil {
			return cids, fmt.Errorf("PutRawMany: batch of %d failed (%v), then one at a time: %w", len(blks), err, firstErr)
		}
	}
	return cids, nil
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
	block, err := dl.blockServ.GetBlock(context.Background(), cid)
	//Make sure it is stored
	// dl.blockServ.AddBlock(context.Background(), block)
	if err != nil {
		return nil, err
	}
	dag, err := dagCbor.Decode(block.RawData(), mh.SHA2_256, -1)
	return dag, err
}

func (dl *DataLayer) GetRaw(cid cid.Cid) ([]byte, error) {
	block, err := dl.blockServ.GetBlock(context.Background(), cid)
	if err != nil {
		return nil, err
	}
	return block.RawData(), nil
}

func (dl *DataLayer) notify(ctx context.Context, block blocks.Block) {
	if dl.bitswap == nil {
		// The LocalOnly view has no network side: a write stays local.
		return
	}
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
