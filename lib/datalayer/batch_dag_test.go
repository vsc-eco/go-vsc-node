package datalayer_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"testing"
	"time"

	DataLayer "vsc-node/lib/datalayer"
	"vsc-node/modules/common/common_types"

	"github.com/ipfs/boxo/blockservice"
	blockstore "github.com/ipfs/boxo/blockstore"
	"github.com/ipfs/boxo/exchange/offline"
	"github.com/ipfs/boxo/ipld/merkledag"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	dssync "github.com/ipfs/go-datastore/sync"
	ipld "github.com/ipfs/go-ipld-format"
	"github.com/stretchr/testify/require"
)

// stateOps applies one random sequence of writes and deletes, the same on
// every DataBin it is given: flat keys, keys in nested directories, rewrites,
// deletes of keys and of whole directories.
type stateOp struct {
	del  bool
	key  string
	link cid.Cid
}

func randomOps(t *testing.T, da *DataLayer.DataLayer, seed int64, n int) []stateOp {
	t.Helper()
	r := rand.New(rand.NewSource(seed))
	ops := make([]stateOp, 0, n)
	keys := make([]string, 0, n)
	for i := 0; i < n; i++ {
		if len(keys) > 0 && r.Intn(8) == 0 {
			k := keys[r.Intn(len(keys))]
			ops = append(ops, stateOp{del: true, key: k})
			continue
		}
		var k string
		switch r.Intn(4) {
		case 0:
			k = fmt.Sprintf("d%d/k%d", r.Intn(5), r.Intn(n))
		case 1:
			k = fmt.Sprintf("d%d/e%d/k%d", r.Intn(3), r.Intn(3), r.Intn(n))
		default:
			k = fmt.Sprintf("k%d", r.Intn(n))
		}
		// A raw block, as a contract's state value is (MakeOutputs).
		c, err := da.PutRaw([]byte(fmt.Sprintf("v-%d-%d", seed, r.Int63())), common_types.PutRawOptions{})
		require.NoError(t, err)
		ops = append(ops, stateOp{key: k, link: *c})
		keys = append(keys, k)
	}
	return ops
}

func apply(t *testing.T, db *DataLayer.DataBin, ops []stateOp) {
	t.Helper()
	for _, op := range ops {
		if op.del {
			db.Delete(op.key)
		} else {
			require.NoError(t, db.Set(op.key, op.link))
		}
	}
}

// isolatedDA is a data layer over its own in-memory block store with no
// network, so a test sees only the nodes its own DataBin wrote.
func isolatedDA() *DataLayer.DataLayer {
	bs := blockstore.NewBlockstore(dssync.MutexWrap(datastore.NewMapDatastore()))
	return DataLayer.NewOffline(merkledag.NewDAGService(blockservice.New(bs, offline.Exchange(bs))))
}

// Batching only changes WHEN a directory's nodes are written, never what they
// are: the root CID of every state, built from empty and rebuilt from a saved
// root, is the CID the unbatched directory computes. A different root here
// would be a consensus fork (every contract's state_merkle).
func TestBatchedStateDirectoryHasTheSameRootAsUnbatched(t *testing.T) {
	da := getDirTestDA(t)
	sizes := []int{0, 1, 2, 5, 10, 30, 300, 1000, 3000}
	if os.Getenv("DATALAYER_BIG") != "" {
		sizes = append(sizes, 20000) // minutes: every value node is its own write
	}
	for _, n := range sizes {
		for seed := int64(1); seed <= 2; seed++ {
			start := time.Now() // wall: timing log only
			ops := randomOps(t, da, seed*1000+int64(n), n)

			batched := DataLayer.NewDataBin(da)
			apply(t, &batched, ops)
			plain := DataLayer.NewDataBinUnbatchedForTest(da)
			apply(t, &plain, ops)
			// cidAndPersist also stores the root: a small (basic) directory's
			// root is not stored by Cid, before or after batching.
			rootB, rootP := cidAndPersist(t, da, &batched), cidAndPersist(t, da, &plain)
			require.Equal(t, rootP, rootB, "n=%d seed=%d: batched root differs from unbatched", n, seed)

			// Reload the saved root and change it again, both ways.
			more := randomOps(t, da, seed*7777+int64(n), n/3+5)
			reB := DataLayer.NewDataBinFromCid(da, rootB)
			apply(t, &reB, more)
			reP := DataLayer.NewDataBinFromCidUnbatchedForTest(da, rootP)
			apply(t, &reP, more)
			require.Equal(t, reP.Cid(), reB.Cid(), "n=%d seed=%d: after reload and more changes the roots differ", n, seed)
			t.Logf("n=%d seed=%d: same root %s (%s)", n, seed, rootB, time.Since(start).Round(time.Millisecond))
		}
	}
}

// A directory switches from a flat (basic) node to a sharded one when its
// links pass HAMTShardingSize (256 bytes, about six keys) and back when they
// fall under it. Step through both switches one key at a time, flat and
// nested keys, and compare the root after every step.
func TestBatchedRootAcrossTheShardSwitch(t *testing.T) {
	da := getDirTestDA(t)
	for _, prefix := range []string{"", "dir/"} {
		batched := DataLayer.NewDataBin(da)
		plain := DataLayer.NewDataBinUnbatchedForTest(da)
		const n = 30
		keys := make([]string, n)
		for i := 0; i < n; i++ {
			keys[i] = fmt.Sprintf("%skey-%02d", prefix, i)
			c, err := da.PutRaw([]byte(fmt.Sprintf("switch-%s-%d", prefix, i)), common_types.PutRawOptions{})
			require.NoError(t, err)
			require.NoError(t, batched.Set(keys[i], *c))
			require.NoError(t, plain.Set(keys[i], *c))
			require.Equal(t, plain.Cid(), batched.Cid(), "prefix %q: after adding %d keys", prefix, i+1)
		}
		for i := n - 1; i >= 0; i-- {
			batched.Delete(keys[i])
			plain.Delete(keys[i])
			require.Equal(t, plain.Cid(), batched.Cid(), "prefix %q: down to %d keys", prefix, i)
		}
	}
}

// After a batched Cid every node of the state is in the block store: on a
// store of its own (so nothing else wrote the same nodes), a local-only read
// of the root that never asks the network finds every key under the CID it
// was written with, and records no failed read. Flat keys, as contracts use
// them.
func TestBatchedSaveStoresEveryNode(t *testing.T) {
	for _, n := range []int{3, 5, 10, 300, 3000} {
		da := isolatedDA()
		r := rand.New(rand.NewSource(4242 + int64(n)))
		db := DataLayer.NewDataBin(da)
		want := map[string]cid.Cid{}
		for i := 0; i < n; i++ {
			k := fmt.Sprintf("k%d", r.Intn(n))
			if r.Intn(8) == 0 {
				db.Delete(k)
				delete(want, k)
				continue
			}
			v := merkledag.NewRawNode([]byte(fmt.Sprintf("v-%d-%d", n, r.Int63())))
			require.NoError(t, da.DagServ.Add(context.Background(), v))
			require.NoError(t, db.Set(k, v.Cid()))
			want[k] = v.Cid()
		}
		root := cidAndPersist(t, da, &db)

		view := da.LocalOnlyTracking()
		reloaded := DataLayer.NewDataBinFromCid(view, root)
		for k, c := range want {
			got, err := reloaded.Get(k)
			require.NoError(t, err, "n=%d: key %q not readable from the local store", n, k)
			require.Equal(t, c, *got, "n=%d: key %q", n, k)
		}
		require.False(t, view.Missed(), "n=%d: a node of the saved state is not in the local store", n)
		t.Logf("n=%d: all %d keys read back from the local store", n, len(want))
	}
}

// A failed batch write keeps the nodes pending: they stay readable from
// memory and the next flush writes them.
func TestBatchedFailedFlushKeepsTheNodes(t *testing.T) {
	under := &failingDAG{DAGService: isolatedDA().DagServ, fail: true}
	b := DataLayer.NewBatchDAGForTest(under)
	n := merkledag.NodeWithData([]byte("pending"))
	require.NoError(t, b.Add(context.Background(), n))
	require.Error(t, DataLayer.FlushForTest(b))
	got, err := b.Get(context.Background(), n.Cid())
	require.NoError(t, err, "a node whose write failed is no longer readable")
	require.Equal(t, n.Cid(), got.Cid())
	under.fail = false
	require.NoError(t, DataLayer.FlushForTest(b))
	_, err = under.DAGService.Get(context.Background(), n.Cid())
	require.NoError(t, err, "the retried flush did not write the node")
}

type failingDAG struct {
	ipld.DAGService
	fail bool
}

func (f *failingDAG) AddMany(ctx context.Context, ns []ipld.Node) error {
	if f.fail {
		return errors.New("disk full")
	}
	return f.DAGService.AddMany(ctx, ns)
}

// PutRawMany stores the same blocks under the same CIDs as one PutRaw each.
func TestPutRawManyMatchesPutRaw(t *testing.T) {
	da := getDirTestDA(t)
	values := [][]byte{[]byte("a"), []byte(""), []byte("same"), []byte("same"), make([]byte, 70000)}
	cids, err := da.PutRawMany(values)
	require.NoError(t, err)
	require.Len(t, cids, len(values))
	for i, v := range values {
		one, err := da.PutRaw(v, common_types.PutRawOptions{})
		require.NoError(t, err)
		require.Equal(t, *one, cids[i], "value %d", i)
		got, err := da.LocalOnly().GetRaw(cids[i])
		require.NoError(t, err)
		require.True(t, bytes.Equal(v, got), "value %d read back as %d bytes, want %d", i, len(got), len(v))
	}
	none, err := da.PutRawMany(nil)
	require.NoError(t, err)
	require.Empty(t, none)
}
