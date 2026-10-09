package datalayer_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	DataLayer "vsc-node/lib/datalayer"

	"github.com/ipfs/boxo/blockservice"
	blockstore "github.com/ipfs/boxo/blockstore"
	"github.com/ipfs/boxo/exchange/offline"
	"github.com/ipfs/boxo/ipld/merkledag"
	uio "github.com/ipfs/boxo/ipld/unixfs/io"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	dsync "github.com/ipfs/go-datastore/sync"
	format "github.com/ipfs/go-ipld-format"
	"github.com/stretchr/testify/require"
)

// flakyDagServ fails its first `failures` Gets (a blockstore fault), then reads
// from the real DAG service.
type flakyDagServ struct {
	format.DAGService
	failures int64
	gets     atomic.Int64
}

func (f *flakyDagServ) Get(ctx context.Context, c cid.Cid) (format.Node, error) {
	if f.gets.Add(1) <= f.failures {
		return nil, errors.New("injected blockstore fault")
	}
	return f.DAGService.Get(ctx, c)
}

// A failed state read must not come back as an empty directory: the contract
// would run against empty state while peers read the real one. The load is
// retried until it succeeds and returns the real state.
func TestStateLoadRetriesInsteadOfReturningEmpty(t *testing.T) {
	ctx := context.Background()
	bs := blockstore.NewBlockstore(dsync.MutexWrap(datastore.NewMapDatastore()))
	dag := merkledag.NewDAGService(blockservice.New(bs, offline.Exchange(bs)))

	value := merkledag.NodeWithData([]byte("balance=42"))
	require.NoError(t, dag.Add(ctx, value))
	dir := uio.NewDirectory(dag)
	require.NoError(t, dir.AddChild(ctx, "balance", value))
	root, err := dir.GetNode()
	require.NoError(t, err)
	require.NoError(t, dag.Add(ctx, root))

	flaky := &flakyDagServ{DAGService: dag, failures: 3}
	da := &DataLayer.DataLayer{DagServ: flaky}

	bin := DataLayer.NewDataBinFromCid(da, root.Cid())

	require.True(t, bin.Has("balance"), "state loaded as empty after a transient read fault")
	require.Greater(t, flaky.gets.Load(), int64(3), "the fault path was not exercised")
	got, err := bin.Get("balance")
	require.NoError(t, err)
	require.Equal(t, value.Cid(), *got)
}
