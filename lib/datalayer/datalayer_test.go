package datalayer_test

import (
	"context"
	"fmt"
	"testing"

	DataLayer "vsc-node/lib/datalayer"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc/witnesses"
	p2pInterface "vsc-node/modules/p2p"

	"github.com/ipfs/boxo/ipld/merkledag"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore/query"
	"github.com/libp2p/go-libp2p"
	kadDht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	rhost "github.com/libp2p/go-libp2p/p2p/host/routed"
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

	identityConfig := common.NewIdentityConfig()
	p2pConfig := p2pInterface.NewConfig()
	sysConfig := systemconfig.MocknetConfig()

	p2p := p2pInterface.New(witnesses.NewEmptyWitnesses(), p2pConfig, identityConfig, sysConfig, nil)

	da := DataLayer.New(p2p)
	m.Log(setupResult.host.ID(), da)
}

//Test cases:
//Pin add (single object)
//Pin rm (single object)
//Pin add (recursive tree object)
//Pin rm (recursive tree object)
//Pin add (recursive shared child tree object)
//Pin rm (common roots, check if child is still exsting)

func TestGC(t *testing.T) {
	identityConfig := common.NewIdentityConfig()
	p2pConfig := p2pInterface.NewConfig()
	sysConfig := systemconfig.MocknetConfig()

	p2p := p2pInterface.New(witnesses.NewEmptyWitnesses(), p2pConfig, identityConfig, sysConfig, nil)
	da := DataLayer.New(p2p)
	a := aggregate.New([]aggregate.Plugin{identityConfig, p2pConfig, p2p, da})
	assert.Nil(t, a.Init())

	gc := DataLayer.NewGC(da.Datastore, da.DagServ)

	// Create local nodes so the test doesn't try to fetch from the network
	childNode := merkledag.NodeWithData([]byte("child-data"))
	err := da.DagServ.Add(context.Background(), childNode)
	assert.Nil(t, err)

	parentNode := merkledag.NodeWithData([]byte("parent-data"))
	parentNode.AddNodeLink("child", childNode)
	err = da.DagServ.Add(context.Background(), parentNode)
	assert.Nil(t, err)

	pinNode := merkledag.NodeWithData([]byte("pinned-data"))
	err = da.DagServ.Add(context.Background(), pinNode)
	assert.Nil(t, err)

	testCid := parentNode.Cid()
	testCid2 := pinNode.Cid()
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

	gc.RmPin(testCid)
	gc.GC(pinList)
}

func TestDir(t *testing.T) {
	identityConfig := common.NewIdentityConfig()
	p2pConfig := p2pInterface.NewConfig()
	sysConfig := systemconfig.MocknetConfig()

	p2p := p2pInterface.New(witnesses.NewEmptyWitnesses(), p2pConfig, identityConfig, sysConfig, nil)
	da := DataLayer.New(p2p)
	a := aggregate.New([]aggregate.Plugin{identityConfig, p2pConfig, p2p, da})
	assert.Nil(t, a.Init())
	db := DataLayer.NewDataBin(da)

	node := merkledag.NodeWithData([]byte("test-data"))
	err := da.DagServ.Add(context.Background(), node)
	assert.Nil(t, err)
	testExample := node.Cid()

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

	err = db.Set("dir-1/test1", testExample)

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

	assert.True(t, savedCid.Defined(), "savedCid should be defined after dir-1 entries")

	db.Delete("dir-1")

	dirList, _ = db.List("")

	//Confirm directory deletion
	assert.Equal(t, []string{"key-01"}, *dirList)

	savedCid = db.Cid()

	assert.True(t, savedCid.Defined(), "savedCid should be defined after dir-1 deletion")

}
