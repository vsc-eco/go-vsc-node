package datalayer

import (
	"bytes"
	"context"
	"fmt"

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
	format "github.com/ipfs/go-ipld-format"

	"vsc-node/lib/utils"
	libp2p "vsc-node/modules/p2p"

	"github.com/libp2p/go-libp2p/core/host"

	goJson "encoding/json"

	dagCbor "github.com/ipfs/go-ipld-cbor"
	kadDht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/multiformats/go-multicodec"
	mh "github.com/multiformats/go-multihash"

	a "vsc-node/modules/aggregate"
)

type DataLayer struct {
	// a.Plugin
	p2pService *libp2p.P2PServer
	bitswap    *bitswap.Bitswap
	dht        *kadDht.IpfsDHT
	host       host.Host
	blockServ  blockservice.BlockService
	DagServ    format.DAGService
	Datastore  *badger.Datastore

	dbPrefix []string
}

type MetricsCtx context.Context

type GetOptions struct {
	NoStore bool
}

type PutRawOptions struct {
	Codec multicodec.Code
	Pin   bool
}

func (dl *DataLayer) Init() error {
	ctx := context.Background()

	var path string

	if len(dl.dbPrefix) > 0 {
		path = "badger-" + dl.dbPrefix[0]
	} else {
		path = "badger"
	}

	ds, err := badger.NewDatastore(path, &badger.DefaultOptions)

	if err != nil {
		panic(err)
	}

	var bstore blockstore.Blockstore = blockstore.NewBlockstore(ds)

	bswapnet := network.NewFromIpfsHost(dl.p2pService.Host)

	// Create Bitswap: a new "discovery" parameter, usually the "contentRouter"
	// which does both discovery and providing.
	bswap := bitswap.New(ctx, bswapnet, dl.p2pService.Dht, bstore)
	// A provider system that handles concurrent provides etc. "contentProvider"
	// is usually the "contentRouter" which does both discovery and providing.
	// "contentProvider" could be used directly without wrapping, but it is recommended
	// to do so to provide more efficiently.
	provider, _ := provider.New(ds, provider.Online(dl.p2pService.Dht))
	// A wrapped providing exchange using the previous exchange and the provider.
	exchange := providing.New(bswap, provider)

	// Finally the blockservice
	blockService := blockservice.New(bstore, exchange)
	dl.blockServ = blockService
	dl.bitswap = bswap

	dl.DagServ = merkledag.NewDAGService(blockService)

	return nil
}

func (dl *DataLayer) Start() *promise.Promise[any] {
	return utils.PromiseResolve[any](nil)
}

func (dl *DataLayer) Stop() error {
	return nil
}

// Will always hash using sha256
func (dl *DataLayer) PutRaw(rawData []byte, options PutRawOptions) (*cid.Cid, error) {
	prefix := cid.Prefix{
		Version:  1,
		Codec:    uint64(multicodec.Raw),
		MhType:   mh.SHA2_256,
		MhLength: -1,
	}
	cid, _ := prefix.Sum(rawData)

	fmt.Println("Connection here for bart", cid)
	blockData, _ := blocks.NewBlockWithCid(rawData, cid)

	dl.blockServ.AddBlock(context.TODO(), blockData)

	if options.Pin {
		//Do pin here
		// dl.AddPin(cid, struct{recursive bool}{ recursive: true})
	}

	return &cid, nil
}

func (dl *DataLayer) PutObject(data interface{}) (*cid.Cid, error) {
	ctx := context.Background()

	jsonBytes, err := goJson.Marshal(data)

	if err != nil {
		return nil, err
	}

	r := bytes.NewReader(jsonBytes)
	block, err := dagCbor.FromJSON(r, mh.SHA2_256, -1)

	if err != nil {
		return nil, err
	}

	// block := blocks.NewBlock(bytes)
	cid := block.Cid()
	dl.bitswap.NotifyNewBlocks(ctx, block)
	//We might need to proactively rebroadcast that we are storing a CID
	dl.p2pService.Dht.Provide(ctx, cid, true)
	dl.DagServ.Add(ctx, block)

	return &cid, nil
}

func (dl *DataLayer) Get(cid cid.Cid, options *GetOptions) (*format.Node, error) {
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
	return &node, nil
	// if options.NoStore {
	// } else {
	// 	//This will automatically store locally
	// 	node, err := dl.DagServ.Get(context.Background(), cid)
	// 	return &node, err
	// }
}

// Gets Object then converts it to Golang type seemlessly
func (dl *DataLayer) GetObject(cid cid.Cid, v any, options GetOptions) error {
	dataNode, err := dl.Get(cid, &options)

	if err != nil {
		return err
	}

	dagNode, err := dagCbor.Decode((*dataNode).RawData(), mh.SHA2_256, -1)
	if err != nil {
		return err
	}

	bytes, err := dagNode.MarshalJSON()

	if err != nil {
		return err
	}

	return goJson.Unmarshal(bytes, v)
}

func (dl *DataLayer) GetDag(cid cid.Cid) (*dagCbor.Node, error) {

	go func() {
		dl.dht.Bootstrap(context.TODO())
		peers, _ := dl.dht.FindProviders(context.Background(), cid)
		for _, peer := range peers {
			dl.host.Connect(context.Background(), peer)
		}
		fmt.Println("TRYING TO PULL IN LOOP")
		dl.bitswap.GetBlock(context.Background(), cid)
		fmt.Println("I PULLED SOMETHING LOL")
	}()
	block, err := dl.blockServ.GetBlock(context.Background(), cid)
	//Make sure it is stored
	// dl.blockServ.AddBlock(context.Background(), block)
	if err != nil {
		return nil, err
	}
	dag, err := dagCbor.Decode(block.RawData(), mh.SHA2_256, -1)
	return dag, err
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

func New(p2pService *libp2p.P2PServer, dbPrefix ...string) *DataLayer {

	return &DataLayer{

		p2pService: p2pService,
		host:       p2pService.Host,
		dht:        p2pService.Dht,
		dbPrefix:   dbPrefix,
	}
}
