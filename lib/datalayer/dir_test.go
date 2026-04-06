package datalayer_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	DataLayer "vsc-node/lib/datalayer"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc/witnesses"
	p2pInterface "vsc-node/modules/p2p"

	"github.com/ipfs/boxo/ipld/merkledag"
	"github.com/ipfs/go-cid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	dirTestDA   *DataLayer.DataLayer
	dirTestOnce sync.Once
)

// getDirTestDA returns a shared DataLayer for dir tests, initializing on first call.
func getDirTestDA(t *testing.T) *DataLayer.DataLayer {
	t.Helper()
	dirTestOnce.Do(func() {
		identityConfig := common.NewIdentityConfig()
		p2pConfig := p2pInterface.NewConfig()
		sysConfig := systemconfig.MocknetConfig()
		p2p := p2pInterface.New(witnesses.NewEmptyWitnesses(), p2pConfig, identityConfig, sysConfig, nil)
		dirTestDA = DataLayer.New(p2p)
		a := aggregate.New([]aggregate.Plugin{identityConfig, p2pConfig, p2p, dirTestDA})
		if err := a.Init(); err != nil {
			t.Fatal("Failed to init DataLayer:", err)
		}
	})
	return dirTestDA
}

// addTestNode stores a raw data node in the DAG service and returns it.
func addTestNode(t *testing.T, da *DataLayer.DataLayer, data string) *merkledag.ProtoNode {
	t.Helper()
	node := merkledag.NodeWithData([]byte(data))
	err := da.DagServ.Add(context.Background(), node)
	require.Nil(t, err)
	return node
}

// cidAndPersist computes the CID of a DataBin and persists the root directory
// node to the blockstore so that NewDataBinFromCid can reload it.
// HAMT directories auto-persist via shard.Node() → dserv.Add, but
// BasicDirectory.GetNode() does not, so we explicitly add the node.
func cidAndPersist(t *testing.T, da *DataLayer.DataLayer, db *DataLayer.DataBin) cid.Cid {
	t.Helper()
	c := db.Cid()
	// After Cid() has compacted and serialized, get the root node and persist it.
	node, err := db.Leaf.Dir.GetNode()
	require.Nil(t, err)
	require.Nil(t, da.DagServ.Add(context.Background(), node))
	return c
}

// TestDeterministicCid verifies that two DataBins with the same entries
// inserted in different orders produce the same CID.
func TestDeterministicCid(t *testing.T) {
	da := getDirTestDA(t)

	nodeA := addTestNode(t, da, "det-value-a")
	nodeB := addTestNode(t, da, "det-value-b")
	nodeC := addTestNode(t, da, "det-value-c")

	// Build DataBin with insertion order: A, B, C
	db1 := DataLayer.NewDataBin(da)
	db1.Set("key-a", nodeA.Cid())
	db1.Set("key-b", nodeB.Cid())
	db1.Set("key-c", nodeC.Cid())
	cid1 := db1.Cid()

	// Build DataBin with insertion order: C, A, B
	db2 := DataLayer.NewDataBin(da)
	db2.Set("key-c", nodeC.Cid())
	db2.Set("key-a", nodeA.Cid())
	db2.Set("key-b", nodeB.Cid())
	cid2 := db2.Cid()

	assert.Equal(t, cid1, cid2, "CIDs should be identical regardless of insertion order")
}

// TestDeterministicCidWithSubdirectories verifies determinism when entries
// span nested subdirectories inserted in different orders.
func TestDeterministicCidWithSubdirectories(t *testing.T) {
	da := getDirTestDA(t)

	nodeA := addTestNode(t, da, "det-sub-value-a")
	nodeB := addTestNode(t, da, "det-sub-value-b")
	nodeC := addTestNode(t, da, "det-sub-value-c")

	// Build DataBin with insertion order: dir-1/a, dir-2/b, root-c
	db1 := DataLayer.NewDataBin(da)
	db1.Set("dir-1/a", nodeA.Cid())
	db1.Set("dir-2/b", nodeB.Cid())
	db1.Set("root-c", nodeC.Cid())
	cid1 := db1.Cid()

	// Build DataBin with reversed order: root-c, dir-2/b, dir-1/a
	db2 := DataLayer.NewDataBin(da)
	db2.Set("root-c", nodeC.Cid())
	db2.Set("dir-2/b", nodeB.Cid())
	db2.Set("dir-1/a", nodeA.Cid())
	cid2 := db2.Cid()

	assert.Equal(t, cid1, cid2, "CIDs should be identical regardless of subdirectory insertion order")
}

// TestDeterministicCidAfterReload verifies that saving a DataBin, reloading
// from CID, applying the same modifications, and saving again produces the
// same CID on both paths. This simulates what happens across nodes: they
// load from the same base CID, apply the same diffs, and should arrive at
// the same result.
func TestDeterministicCidAfterReload(t *testing.T) {
	da := getDirTestDA(t)

	node1 := addTestNode(t, da, "det-reload-1")
	node2 := addTestNode(t, da, "det-reload-2")
	node3 := addTestNode(t, da, "det-reload-3")

	// Create initial state and persist to blockstore
	dbInit := DataLayer.NewDataBin(da)
	dbInit.Set("existing-1", node1.Cid())
	dbInit.Set("existing-2", node2.Cid())
	baseCid := cidAndPersist(t, da, &dbInit)

	t.Log("Base CID:", baseCid)

	// Simulate node A: load from CID, apply diff
	dbA, err := DataLayer.NewDataBinFromCid(da, baseCid)
	assert.NoError(t, err)
	dbA.Set("new-key", node3.Cid())
	cidA := dbA.Cid()

	// Simulate node B: load from same CID, apply same diff
	dbB, err := DataLayer.NewDataBinFromCid(da, baseCid)
	assert.NoError(t, err)
	dbB.Set("new-key", node3.Cid())
	cidB := dbB.Cid()

	assert.Equal(t, cidA, cidB, "Two nodes loading from same base and applying same diff should produce same CID")
}

// TestDeterministicCidAfterDeletion verifies that deleting entries and
// re-serializing produces deterministic CIDs.
func TestDeterministicCidAfterDeletion(t *testing.T) {
	da := getDirTestDA(t)

	nodeA := addTestNode(t, da, "det-del-a")
	nodeB := addTestNode(t, da, "det-del-b")
	nodeC := addTestNode(t, da, "det-del-c")

	// Path 1: insert A, B, C then delete B
	db1 := DataLayer.NewDataBin(da)
	db1.Set("key-a", nodeA.Cid())
	db1.Set("key-b", nodeB.Cid())
	db1.Set("key-c", nodeC.Cid())
	db1.Delete("key-b")
	cid1 := db1.Cid()

	// Path 2: insert C, A (never insert B)
	db2 := DataLayer.NewDataBin(da)
	db2.Set("key-c", nodeC.Cid())
	db2.Set("key-a", nodeA.Cid())
	cid2 := db2.Cid()

	assert.Equal(t, cid1, cid2, "Deleting an entry should produce same CID as never inserting it")
}

// TestDeterministicCidManyKeys verifies determinism with enough keys to
// trigger HAMT sharding (threshold is 256).
func TestDeterministicCidManyKeys(t *testing.T) {
	da := getDirTestDA(t)

	const numKeys = 300 // Above HAMTShardingSize=256

	// Create all value nodes first
	nodes := make([]*merkledag.ProtoNode, numKeys)
	for i := 0; i < numKeys; i++ {
		nodes[i] = addTestNode(t, da, fmt.Sprintf("many-value-%d", i))
	}

	// Build DataBin inserting in forward order (0..299)
	db1 := DataLayer.NewDataBin(da)
	for i := 0; i < numKeys; i++ {
		key := fmt.Sprintf("key-%04d", i)
		db1.Set(key, nodes[i].Cid())
	}
	cid1 := db1.Cid()

	// Build DataBin inserting in reverse order (299..0)
	db2 := DataLayer.NewDataBin(da)
	for i := numKeys - 1; i >= 0; i-- {
		key := fmt.Sprintf("key-%04d", i)
		db2.Set(key, nodes[i].Cid())
	}
	cid2 := db2.Cid()

	assert.Equal(t, cid1, cid2,
		"CIDs should be identical for %d keys regardless of insertion order (HAMT threshold=256)", numKeys)
}

// TestDeterministicCidManyKeysReloadAndModify loads a large HAMT-backed
// DataBin from CID, modifies a subset of keys, and verifies that two
// independent loads + identical modifications produce the same CID.
// This is the exact scenario that was non-deterministic before the fix.
func TestDeterministicCidManyKeysReloadAndModify(t *testing.T) {
	da := getDirTestDA(t)

	const numKeys = 3000

	// Create value nodes
	nodes := make([]*merkledag.ProtoNode, numKeys+10)
	for i := 0; i < numKeys+10; i++ {
		nodes[i] = addTestNode(t, da, fmt.Sprintf("reload-value-%d", i))
	}

	// Build initial large state — HAMT auto-persists all shard nodes
	// via shard.Node() → dserv.Add during Cid() serialization.
	dbInit := DataLayer.NewDataBin(da)
	for i := 0; i < numKeys; i++ {
		key := fmt.Sprintf("key-%04d", i)
		dbInit.Set(key, nodes[i].Cid())
	}
	baseCid := dbInit.Cid()

	t.Log("Base CID (many keys):", baseCid)

	// Simulate node A: load, modify a few keys
	dbA, err := DataLayer.NewDataBinFromCid(da, baseCid)
	assert.NoError(t, err)
	dbA.Set("key-0010", nodes[numKeys].Cid())    // update existing
	dbA.Set("key-0050", nodes[numKeys+1].Cid())  // update existing
	dbA.Delete("key-0100")                       // delete
	dbA.Set("key-new-1", nodes[numKeys+2].Cid()) // add new
	cidA := dbA.Cid()

	// Simulate node B: load same base, apply same modifications
	dbB, err := DataLayer.NewDataBinFromCid(da, baseCid)
	assert.NoError(t, err)
	dbB.Set("key-0010", nodes[numKeys].Cid())
	dbB.Set("key-0050", nodes[numKeys+1].Cid())
	dbB.Delete("key-0100")
	dbB.Set("key-new-1", nodes[numKeys+2].Cid())
	cidB := dbB.Cid()

	assert.Equal(t, cidA, cidB,
		"Two nodes loading same HAMT base and applying same diffs should produce identical CIDs")
}
