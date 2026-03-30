package datalayer

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc/witnesses"
	p2pInterface "vsc-node/modules/p2p"

	"github.com/ipfs/go-cid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	dirTestDA   *DataLayer
	dirTestOnce sync.Once
)

// getDirTestDA returns a shared DataLayer for dir tests, initializing on first call.
func getDirTestDA(t *testing.T) *DataLayer {
	t.Helper()
	dirTestOnce.Do(func() {
		identityConfig := common.NewIdentityConfig()
		p2pConfig := p2pInterface.NewConfig()
		sysConfig := systemconfig.MocknetConfig()
		p2p := p2pInterface.New(witnesses.NewEmptyWitnesses(), p2pConfig, identityConfig, sysConfig, nil)
		dirTestDA = New(p2p)
		a := aggregate.New([]aggregate.Plugin{identityConfig, p2pConfig, p2p, dirTestDA})
		if err := a.Init(); err != nil {
			t.Fatal("Failed to init DataLayer:", err)
		}
	})
	return dirTestDA
}

// addTestNode stores a CBOR-encoded value in the DAG service and returns its
// CID. Uses DagCbor codec like real contract state (PutObject), so that the
// DagPb codec check in newLeafFromCid correctly distinguishes data from dirs.
func addTestNode(t *testing.T, da *DataLayer, data string) cid.Cid {
	t.Helper()
	c, err := da.PutObject(map[string]string{"d": data})
	require.NoError(t, err)
	return *c
}

// cidAndPersist computes the CID of a DataBin and persists all directory
// nodes to the blockstore so that NewDataBinFromCid can reload it.
// HAMT directories auto-persist internal shards via shard.Node() → dserv.Add,
// but BasicDirectory.GetNode() does not, so we explicitly add nodes.
func cidAndPersist(t *testing.T, da *DataLayer, db *DataBin) cid.Cid {
	t.Helper()
	c := db.Cid()
	// Persist all subdirectory nodes, then the root.
	persistLeaf(t, da, &db.Leaf)
	return c
}

// persistLeaf recursively persists all directory nodes in a LeafDir tree.
func persistLeaf(t *testing.T, da *DataLayer, lf *LeafDir) {
	t.Helper()
	for _, child := range lf.leaves {
		persistLeaf(t, da, child)
	}
	node, err := lf.Dir.GetNode()
	require.Nil(t, err)
	require.Nil(t, da.DagServ.Add(context.Background(), node))
}

// TestDeterministicCid verifies that two DataBins with the same entries
// inserted in different orders produce the same CID.
func TestDeterministicCid(t *testing.T) {
	da := getDirTestDA(t)

	cidA := addTestNode(t, da, "det-value-a")
	cidB := addTestNode(t, da, "det-value-b")
	cidC := addTestNode(t, da, "det-value-c")

	// Build DataBin with insertion order: A, B, C
	db1 := NewDataBin(da)
	db1.Set("key-a", cidA)
	db1.Set("key-b", cidB)
	db1.Set("key-c", cidC)
	cid1 := db1.Cid()

	// Build DataBin with insertion order: C, A, B
	db2 := NewDataBin(da)
	db2.Set("key-c", cidC)
	db2.Set("key-a", cidA)
	db2.Set("key-b", cidB)
	cid2 := db2.Cid()

	assert.Equal(t, cid1, cid2, "CIDs should be identical regardless of insertion order")
}

// TestDeterministicCidWithSubdirectories verifies determinism when entries
// span nested subdirectories inserted in different orders.
func TestDeterministicCidWithSubdirectories(t *testing.T) {
	da := getDirTestDA(t)

	cidA := addTestNode(t, da, "det-sub-value-a")
	cidB := addTestNode(t, da, "det-sub-value-b")
	cidC := addTestNode(t, da, "det-sub-value-c")

	// Build DataBin with insertion order: dir-1/a, dir-2/b, root-c
	db1 := NewDataBin(da)
	db1.Set("dir-1/a", cidA)
	db1.Set("dir-2/b", cidB)
	db1.Set("root-c", cidC)
	cid1 := db1.Cid()

	// Build DataBin with reversed order: root-c, dir-2/b, dir-1/a
	db2 := NewDataBin(da)
	db2.Set("root-c", cidC)
	db2.Set("dir-2/b", cidB)
	db2.Set("dir-1/a", cidA)
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

	cid1 := addTestNode(t, da, "det-reload-1")
	cid2 := addTestNode(t, da, "det-reload-2")
	cid3 := addTestNode(t, da, "det-reload-3")

	// Create initial state and persist to blockstore
	dbInit := NewDataBin(da)
	dbInit.Set("existing-1", cid1)
	dbInit.Set("existing-2", cid2)
	baseCid := cidAndPersist(t, da, &dbInit)

	t.Log("Base CID:", baseCid)

	// Simulate node A: load from CID, apply diff
	dbA := NewDataBinFromCid(da, baseCid)
	dbA.Set("new-key", cid3)
	cidA := dbA.Cid()

	// Simulate node B: load from same CID, apply same diff
	dbB := NewDataBinFromCid(da, baseCid)
	dbB.Set("new-key", cid3)
	cidB := dbB.Cid()

	assert.Equal(t, cidA, cidB, "Two nodes loading from same base and applying same diff should produce same CID")
}

// TestDeterministicCidAfterDeletion verifies that deleting entries and
// re-serializing produces deterministic CIDs.
func TestDeterministicCidAfterDeletion(t *testing.T) {
	da := getDirTestDA(t)

	cidA := addTestNode(t, da, "det-del-a")
	cidB := addTestNode(t, da, "det-del-b")
	cidC := addTestNode(t, da, "det-del-c")

	// Path 1: insert A, B, C then delete B
	db1 := NewDataBin(da)
	db1.Set("key-a", cidA)
	db1.Set("key-b", cidB)
	db1.Set("key-c", cidC)
	db1.Delete("key-b")
	cid1 := db1.Cid()

	// Path 2: insert C, A (never insert B)
	db2 := NewDataBin(da)
	db2.Set("key-c", cidC)
	db2.Set("key-a", cidA)
	cid2 := db2.Cid()

	assert.Equal(t, cid1, cid2, "Deleting an entry should produce same CID as never inserting it")
}

// TestDeterministicCidManyKeys verifies determinism with enough keys to
// trigger HAMT sharding (threshold is 256).
func TestDeterministicCidManyKeys(t *testing.T) {
	da := getDirTestDA(t)

	const numKeys = 300 // Above HAMTShardingSize=256

	// Create all value nodes first
	cids := make([]cid.Cid, numKeys)
	for i := 0; i < numKeys; i++ {
		cids[i] = addTestNode(t, da, fmt.Sprintf("many-value-%d", i))
	}

	// Build DataBin inserting in forward order (0..299)
	db1 := NewDataBin(da)
	for i := 0; i < numKeys; i++ {
		key := fmt.Sprintf("key-%04d", i)
		db1.Set(key, cids[i])
	}
	cid1 := db1.Cid()

	// Build DataBin inserting in reverse order (299..0)
	db2 := NewDataBin(da)
	for i := numKeys - 1; i >= 0; i-- {
		key := fmt.Sprintf("key-%04d", i)
		db2.Set(key, cids[i])
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
	cids := make([]cid.Cid, numKeys+10)
	for i := 0; i < numKeys+10; i++ {
		cids[i] = addTestNode(t, da, fmt.Sprintf("reload-value-%d", i))
	}

	// Build initial large state — HAMT auto-persists all shard nodes
	// via shard.Node() → dserv.Add during Cid() serialization.
	dbInit := NewDataBin(da)
	for i := 0; i < numKeys; i++ {
		key := fmt.Sprintf("key-%04d", i)
		dbInit.Set(key, cids[i])
	}
	baseCid := dbInit.Cid()

	t.Log("Base CID (many keys):", baseCid)

	// Simulate node A: load, modify a few keys
	dbA := NewDataBinFromCid(da, baseCid)
	dbA.Set("key-0010", cids[numKeys])    // update existing
	dbA.Set("key-0050", cids[numKeys+1])  // update existing
	dbA.Delete("key-0100")                // delete
	dbA.Set("key-new-1", cids[numKeys+2]) // add new
	cidA := dbA.Cid()

	// Simulate node B: load same base, apply same modifications
	dbB := NewDataBinFromCid(da, baseCid)
	dbB.Set("key-0010", cids[numKeys])
	dbB.Set("key-0050", cids[numKeys+1])
	dbB.Delete("key-0100")
	dbB.Set("key-new-1", cids[numKeys+2])
	cidB := dbB.Cid()

	assert.Equal(t, cidA, cidB,
		"Two nodes loading same HAMT base and applying same diffs should produce identical CIDs")
}

// TestHAMTShardPersistence verifies that all key-value pairs survive a
// save→reload round trip when the directory is large enough to produce
// multi-level HAMT shards.
func TestHAMTShardPersistence(t *testing.T) {
	da := getDirTestDA(t)

	const numKeys = 300

	cids := make([]cid.Cid, numKeys)
	for i := 0; i < numKeys; i++ {
		cids[i] = addTestNode(t, da, fmt.Sprintf("shard-persist-%d", i))
	}

	db := NewDataBin(da)
	for i := 0; i < numKeys; i++ {
		db.Set(fmt.Sprintf("k%04d", i), cids[i])
	}

	savedCid := cidAndPersist(t, da, &db)

	reloaded := NewDataBinFromCid(da, savedCid)

	for i := 0; i < numKeys; i++ {
		key := fmt.Sprintf("k%04d", i)
		got, err := reloaded.Get(key)
		require.NoError(t, err, "key %s not found after reload", key)
		assert.Equal(t, cids[i], *got, "key %s CID mismatch", key)
	}
}

// TestNestedPathPersistence verifies that keys with "/" paths survive a
// Save→reload round trip. This exercises the addLeafBlocks fix: without it,
// intermediate directory nodes are never written to the blockstore and
// all nested keys are silently lost on reload.
func TestNestedPathPersistence(t *testing.T) {
	da := getDirTestDA(t)

	cidA := addTestNode(t, da, "nested-persist-a")
	cidB := addTestNode(t, da, "nested-persist-b")
	cidC := addTestNode(t, da, "nested-persist-c")

	db := NewDataBin(da)
	db.Set("dir1/a", cidA)
	db.Set("dir1/b", cidB)
	db.Set("dir2/c", cidC)

	savedCid := cidAndPersist(t, da, &db)

	reloaded := NewDataBinFromCid(da, savedCid)

	gotA, err := reloaded.Get("dir1/a")
	require.NoError(t, err, "dir1/a should survive reload")
	assert.Equal(t, cidA, *gotA)

	gotB, err := reloaded.Get("dir1/b")
	require.NoError(t, err, "dir1/b should survive reload")
	assert.Equal(t, cidB, *gotB)

	gotC, err := reloaded.Get("dir2/c")
	require.NoError(t, err, "dir2/c should survive reload")
	assert.Equal(t, cidC, *gotC)
}
