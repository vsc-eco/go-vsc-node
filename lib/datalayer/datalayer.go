package datalayer

import (
	"bytes"
	"context"
	"fmt"

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

	"github.com/libp2p/go-libp2p/core/host"

	goJson "encoding/json"

	dagCbor "github.com/ipfs/go-ipld-cbor"
	kadDht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/multiformats/go-multicodec"
	mh "github.com/multiformats/go-multihash"
)

type DataLayer struct {
	bitswap   bitswap.Bitswap
	host      host.Host
	dht       *kadDht.IpfsDHT
	blockServ blockservice.BlockService
	DagServ   format.DAGService
	Datastore *badger.Datastore
}

type MetricsCtx context.Context

type GetOptions struct {
	NoStore bool
}

type PutRawOptions struct {
	Codec multicodec.Code
	Pin   bool
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
	dl.dht.Provide(ctx, cid, true)
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
	dl.host.ID()

	return make([]peer.ID, 0)
}

func New(host host.Host, dht *kadDht.IpfsDHT, dbPrefix ...string) *DataLayer {

	var ctx context.Context = context.Background()

	var path string

	if len(dbPrefix) > 0 {
		path = "badger-" + dbPrefix[0]
	} else {
		path = "badger"
	}

	ds, eerrBad := badger.NewDatastore(path, &badger.DefaultOptions)
	var bstore blockstore.Blockstore = blockstore.NewBlockstore(ds)

	bswapnet := network.NewFromIpfsHost(host)
	// Create Bitswap: a new "discovery" parameter, usually the "contentRouter"
	// which does both discovery and providing.
	bswap := bitswap.New(ctx, bswapnet, dht, bstore)
	// A provider system that handles concurrent provides etc. "contentProvider"
	// is usually the "contentRouter" which does both discovery and providing.
	// "contentProvider" could be used directly without wrapping, but it is recommended
	// to do so to provide more efficiently.
	provider, err := provider.New(ds, provider.Online(dht))
	// A wrapped providing exchange using the previous exchange and the provider.
	exchange := providing.New(bswap, provider)

	// Finally the blockservice
	blockService := blockservice.New(bstore, exchange)

	fmt.Println("PeerId", host.ID(), err, eerrBad)

	// fx.Lifecycle.
	// cid, _ := cid.Parse("bafyreic5eyutoi7gattebiutaj4prdeev7jwyyeba2hpdnmzxb2cuk4hge")
	// block, _ := blockService.GetBlock(ctx, cid)
	// fmt.Println(block.RawData())

	// exchange.NotifyNewBlocks(ctx, block)
	// newCid := blocks.NewBlock(block.RawData()).Cid()
	// fmt.Println(newCid)
	// dagServ := merkledag.NewDAGService(blockService)
	// dagServ.Add(ctx, )
	// dagNode, _ := dagServ.Get(ctx, cid)

	// dagNode.RawData()
	// decodedObj := struct {
	// 	__t         string
	// 	Merkle_root string
	// }{}
	// decodedObj2 := make(map[string]interface{})
	// dagCbor.DecodeInto(dagNode.RawData(), &decodedObj)
	// dagCbor.DecodeInto(dagNode.RawData(), &decodedObj2)

	// fmt.Println("decodedObj", decodedObj, decodedObj)
	// fmt.Println("decodedObj2", decodedObj2, decodedObj2["__t"], decodedObj2["merkle_root"])
	// fmt.Println("DagNode", dagNode.Links(), dagNode)

	// cid.Prefix()
	// fmt.Println(dagNode.Size())
	// for _, val := range dagNode.Links() {
	// 	fmt.Println(*val)
	// }

	// obj := map[string]interface{}{
	// 	"foo":   "test object",
	// 	"hello": 123,
	// 	"link":  cid,
	// }

	// cborNode, _ := dagCbor.WrapObject(obj, mh.SHA2_256, -1)
	// fmt.Println(cborNode.Cid())
	// dagServ.Add(ctx, cborNode)
	// dht.Provide(ctx, cborNode.Cid(), true)
	// addrsInfo, _ := dht.FindProviders(ctx, cborNode.Cid())

	// for _, value := range addrsInfo {
	// 	fmt.Println("providerInfo", value)
	// }

	// multicodec.DagCbor.String()

	// testBytes := []byte("brat")
	// val := cid.Prefix{
	// 	Version:  1,
	// 	Codec:    uint64(multicodec.Raw),
	// 	MhType:   mh.SHA2_256,
	// 	MhLength: -1,
	// }
	// cid, _ := val.Sum(testBytes)

	// fmt.Println("Connection here for bart", cid)
	// block2, _ := blocks.NewBlockWithCid(testBytes, cid)

	// blockService.AddBlock(context.TODO(), block2)

	return &DataLayer{
		bitswap:   *bswap,
		host:      host,
		dht:       dht,
		blockServ: blockService,
		DagServ:   merkledag.NewDAGService(blockService),
		Datastore: ds,
	}
}
