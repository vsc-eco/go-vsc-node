package datalayer

import (
	"bytes"
	"context"
	"fmt"

	"github.com/libp2p/go-libp2p/core/peer"

	bitswap "github.com/ipfs/boxo/bitswap"
	bsnet "github.com/ipfs/boxo/bitswap/network"
	"github.com/ipfs/boxo/blockservice"
	blockstore "github.com/ipfs/boxo/blockstore"
	"github.com/ipfs/boxo/datastore/dshelp"
	"github.com/ipfs/boxo/ipld/merkledag"
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

func (dl *DataLayer) Get(cid cid.Cid, options GetOptions) (*format.Node, error) {
	if options.NoStore {
		//This is using direct bitswap access which may not use a block store.
		//Thus, it will not store anything upon request.
		block, err := dl.bitswap.GetBlock(context.Background(), cid)
		if err != nil {
			return nil, err
		}
		node, err := dagCbor.DecodeBlock(block)

		if err != nil {
			return nil, err
		}
		return &node, nil
	} else {
		//This will automatically store locally
		node, err := dl.DagServ.Get(context.Background(), cid)
		return &node, err
	}
}

// Gets Object then converts it to Golang type seemlessly
func (dl *DataLayer) GetObject(cid cid.Cid, v any, options GetOptions) error {
	dataNode, err := dl.Get(cid, options)

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

type TransactionHeader struct {
	Nonce         int64    `json:"nonce"`
	RequiredAuths []string `json:"required_auths" jsonschema:"required"`
}

type TransactionContainer struct {
	Type    string `json:"__t" jsonschema:"required"`
	Version string `json:"__v" jsonschema:"required"`

	Headers TransactionHeader `json:"headers"`

	PurposeLink cid.Cid

	//This this can be any kind of object.
	Tx map[string]interface{} `json:"tx"`
}

func (tx *TransactionContainer) Encode() (*[]byte, error) {

	jsonBytes, err := goJson.Marshal(tx)
	fmt.Println(string(jsonBytes))

	fmt.Println(err)
	r := bytes.NewReader(jsonBytes)
	dagNode, err := dagCbor.FromJSON(r, mh.SHA2_256, -1)

	if err != nil {
		return nil, err
	}

	// node, err := dagCbor.WrapObject(tx, mh.SHA2_256, -1)
	// fmt.Println(err)
	bytes := dagNode.RawData()
	return &bytes, nil
}

func (tx *TransactionContainer) Decode(rawData []byte) error {

	dagNode, _ := dagCbor.Decode(rawData, mh.SHA2_256, -1)

	bytes, err := dagNode.MarshalJSON()
	if err != nil {
		return err
	}
	return goJson.Unmarshal(bytes, tx)
}

func (tx *TransactionContainer) ToBlock() (*blocks.BasicBlock, error) {
	jsonBytes, err := goJson.Marshal(tx)

	if err != nil {
		return nil, err
	}

	r := bytes.NewReader(jsonBytes)
	block, _ := dagCbor.FromJSON(r, mh.SHA2_256, -1)

	blk, err := blocks.NewBlockWithCid(block.RawData(), block.Cid())
	return blk, err
}

func (dl *DataLayer) FindProviders(cid.Cid) []peer.ID {
	dl.host.ID()

	return make([]peer.ID, 0)
}

func New(host host.Host, dht *kadDht.IpfsDHT) *DataLayer {

	var ctx context.Context = context.Background()

	ds, _ := badger.NewDatastore("badger", &badger.DefaultOptions)
	var bstore blockstore.Blockstore = blockstore.NewBlockstore(ds)

	network := bsnet.NewFromIpfsHost(host, dht)
	exchange := bitswap.New(ctx, network, bstore)
	// exchange.GetBlock()
	blockService := blockservice.New(bstore, exchange)

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
		bitswap:   *exchange,
		host:      host,
		dht:       dht,
		blockServ: blockService,
		DagServ:   merkledag.NewDAGService(blockService),
		Datastore: ds,
	}
}
