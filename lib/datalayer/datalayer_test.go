package datalayer_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	DataLayer "vsc-node/lib/datalayer"

	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore/query"
	dagCbor "github.com/ipfs/go-ipld-cbor"
	"github.com/libp2p/go-libp2p"
	kadDht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	rhost "github.com/libp2p/go-libp2p/p2p/host/routed"
	mh "github.com/multiformats/go-multihash"
	"github.com/stretchr/testify/assert"
)

var BOOTSTRAP = []string{
	"/ip4/127.0.0.1/tcp/4001/p2p/12D3KooWAvxZcLJmZVUaoAtey28REvaBwxvfTvQfxWtXJ2fpqWnw",
	"/dnsaddr/bootstrap.libp2p.io/p2p/QmNnooDu7bfjPFoTZYxMNLWUQJyrVwtbZg5gBMjTezGAJN",
	"/dnsaddr/bootstrap.libp2p.io/p2p/QmQCU2EcMqAqQPR2i9bChDtGNJchTbq5TbXJJ16u19uLTa",
	"/dnsaddr/bootstrap.libp2p.io/p2p/QmbLHAnMoJPWSCR5Zhtx6BHJX9KiKNN6tpvbUcqanj75Nb",
	"/dnsaddr/bootstrap.libp2p.io/p2p/QmcZf59bWwK5XFi76CZX8cbJ4BhTzzA3gU1ZjYZcYW3dwt",
	"/ip4/104.131.131.82/tcp/4001/p2p/QmaCpDMGvV2BGHeYERUEnRQAwe3N8SzbUtfsmvsqQLuvuJ",         // mars.i.ipfs.io
	"/ip4/104.131.131.82/udp/4001/quic-v1/p2p/QmaCpDMGvV2BGHeYERUEnRQAwe3N8SzbUtfsmvsqQLuvuJ", // mars.i.ipfs.io
}

type setupResult struct {
	host host.Host
	dht  *kadDht.IpfsDHT
}

func setupEnv() setupResult {
	host, _ := libp2p.New()
	ctx := context.Background()

	dht, _ := kadDht.New(ctx, host)
	routedHost := rhost.Wrap(host, dht)
	go func() {
		dht.Bootstrap(ctx)
		for _, peerStr := range BOOTSTRAP {
			peerId, _ := peer.AddrInfoFromString(peerStr)

			routedHost.Connect(ctx, *peerId)
		}
	}()

	return setupResult{
		host: routedHost,
		dht:  dht,
	}
}

func TestMain(m *testing.T) {
	setupResult := setupEnv()

	time.Sleep(time.Second * 10)

	da := DataLayer.New(setupResult.host, setupResult.dht)
	fmt.Println(setupResult.host.ID(), da)

}

func TestEncoding(t *testing.T) {

	cid, _ := cid.Parse("bafyreiggfvzabljn7izap6w252mgcaezcqdnnfemgkwth5cvtzigutxvgi")
	tx := DataLayer.TransactionContainer{

		Type:    "vsc-tx",
		Version: "1.0",
		Headers: DataLayer.TransactionHeader{
			RequiredAuths: []string{"did:pkh:eip155:1:0x65fA35f2b098d770c9b2E0D2A541b9bB48F6a37a"},
			Nonce:         1,
		},
		PurposeLink: cid,

		Tx: map[string]interface{}{
			"op":   "transfer",
			"to":   "hive:vaultec",
			"from": "did:pkh:eip155:1:0x65fA35f2b098d770c9b2E0D2A541b9bB48F6a37a",
			"amt":  1000,
			"tk":   "HIVE",

			"rawLink2": map[string]interface{}{
				"/": "bafyreiggfvzabljn7izap6w252mgcaezcqdnnfemgkwth5cvtzigutxvgi",
			},
		},
	}

	txBytes, _ := tx.Encode()

	fmt.Println("bytes", string(*txBytes))

	newTx := DataLayer.TransactionContainer{}
	newTx.Decode(*txBytes)
	fmt.Println(newTx.PurposeLink.Prefix())

	cbor := []byte{
		0x3c, 0x61, 0x20, 0x68, 0x72, 0x65, 0x66, 0x3d, 0x22, 0x68, 0x74, 0x74,
		0x70, 0x3a, 0x2f, 0x2f, 0x62, 0x61, 0x66, 0x79, 0x72, 0x65, 0x69, 0x62,
		0x76, 0x68, 0x67, 0x78, 0x66, 0x33, 0x6f, 0x7a, 0x7a, 0x75, 0x77, 0x65,
		0x61, 0x61, 0x69, 0x65, 0x6f, 0x66, 0x37, 0x76, 0x6a, 0x7a, 0x7a, 0x67,
		0x6e, 0x64, 0x71, 0x78, 0x7a, 0x35, 0x6f, 0x70, 0x63, 0x61, 0x61, 0x75,
		0x78, 0x71, 0x76, 0x67, 0x37, 0x79, 0x6e, 0x35, 0x6c, 0x76, 0x6e, 0x78,
		0x37, 0x34, 0x6d, 0x2e, 0x69, 0x70, 0x66, 0x73, 0x2e, 0x6c, 0x6f, 0x63,
		0x61, 0x6c, 0x68, 0x6f, 0x73, 0x74, 0x3a, 0x38, 0x30, 0x38, 0x30, 0x2f,
		0x22, 0x3e, 0x4d, 0x6f, 0x76, 0x65, 0x64, 0x20, 0x50, 0x65, 0x72, 0x6d,
		0x61, 0x6e, 0x65, 0x6e, 0x74, 0x6c, 0x79, 0x3c, 0x2f, 0x61, 0x3e, 0x2e,
		0x0a, 0x0a,
	}
	// r := bytes.NewReader(cbor)
	dagCbor.Decode(cbor, mh.SHA2_256, -1)
}

//Test cases:
//Pin add (single object)
//Pin rm (single object)
//Pin add (recursive tree object)
//Pin rm (recursive tree object)
//Pin add (recursive shared child tree object)
//Pin rm (common roots, check if child is still exsting)

func TestGC(t *testing.T) {

	setup := setupEnv()
	da := DataLayer.New(setup.host, setup.dht)
	//da.NewSet()

	gc := DataLayer.NewGC(da.Datastore, da.DagServ)

	testCid := cid.MustParse("bafybeidj3efmyeaj6xnynljmfuy7f7v7crmbkmzbrttts7fmwe6p75oh5i")
	testCid2 := cid.MustParse("QmQPeNsJPyVWPFDVHb77w8G42Fvo15z4bG2X8D2GhfbSXc")
	pinList := make([]cid.Cid, 0)
	pinList = append(pinList, testCid2)
	fmt.Println("Running GC tests")
	gc.AddPin(testCid, true)

	results, _ := gc.Ds.Query(context.TODO(), query.Query{
		Prefix: "/gc-links",
	})
	for {
		res, more := results.NextSync()

		fmt.Println("Added Key", res.Key)
		if more == false {
			fmt.Println("DONE")
			break
		}
	}

	testExample := cid.MustParse("bafybeidj3efmyeaj6xnynljmfuy7f7v7crmbkmzbrttts7fmwe6p75oh5i")
	fmt.Println(testExample)

	gc.GC(pinList)
	gc.RmPin(testCid)

	// results, _ = gc.Ds.Query(context.TODO(), query.Query{
	// 	Prefix: "/gc-links",
	// })

	// for {
	// 	res, more := results.NextSync()

	// 	fmt.Println("RM key", res.Key)
	// 	if more == false {
	// 		fmt.Println("DONE")
	// 		break
	// 	}
	// }

	gc.GC(pinList)
}

func TestDir(t *testing.T) {
	setup := setupEnv()
	da := DataLayer.New(setup.host, setup.dht)
	db := DataLayer.NewDataBin(da)

	testExample := cid.MustParse("bafybeidj3efmyeaj6xnynljmfuy7f7v7crmbkmzbrttts7fmwe6p75oh5i")

	// fmt.Println("Before CID", db.Cid())

	db.Set("key-01", testExample)

	testCid, _ := db.Get("key-01")

	assert.Equal(t, testExample, *testCid)

	hasTest := db.Has("key-01")

	assert.Equalf(t, hasTest, true, "Key-01 does not exist")

	//Should be false because it doesn't exist
	hasTest = db.Has("not-exists")

	assert.Equal(t, false, hasTest)

	list, _ := db.List("")

	assert.Equal(t, []string{"key-01"}, *list)

	//Add Directory

	err := db.Set("dir-1/test1", testExample)

	assert.Equal(t, nil, err)

	dirHas := db.Has("dir-1/test1")

	assert.Equal(t, true, dirHas)

	dirList, err := db.List("dir-1")

	assert.Equal(t, nil, err)
	assert.Equal(t, []string{"test1"}, *dirList)

	//Add a few more
	db.Set("dir-1/test2", testExample)
	db.Set("dir-1/test3", testExample)
	db.Set("dir-1/test4", testExample)

	dirList, _ = db.List("dir-1")

	//Must be in order
	assert.Equal(t, []string{"test1", "test2", "test3", "test4"}, *dirList)

	//Remove entry from directory

	DeleteExists, err := db.Delete("dir-1/test2")

	assert.Equal(t, true, DeleteExists)
	assert.Equal(t, nil, err)

	DelHas := db.Has("dir-1/test2")

	assert.Equal(t, false, DelHas)

	dirList, _ = db.List("dir-1")
	assert.Equal(t, []string{"test1", "test3", "test4"}, *dirList)

	savedCid := db.Cid()

	assert.Equal(t, cid.MustParse("QmXumDFA3diTx2A2iBbKL2UXKjamhMMNnYRGjRxsUG4Emr"), savedCid)

	db.Delete("dir-1")

	dirList, _ = db.List("")

	//Confirm directory deletion
	assert.Equal(t, []string{"key-01"}, *dirList)

	savedCid = db.Cid()

	assert.Equal(t, cid.MustParse("QmWD6cNC9uvTWxkFdiMLetTKxLzbyW86XjaNwtTRYaTgfK"), savedCid)

}
