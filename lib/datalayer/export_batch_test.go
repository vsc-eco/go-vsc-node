package datalayer

import (
	"context"

	uio "github.com/ipfs/boxo/ipld/unixfs/io"
	"github.com/ipfs/go-cid"
	ipld "github.com/ipfs/go-ipld-format"
)

func newPlainDirForTest(da *DataLayer) uio.Directory { return uio.NewDirectory(da.DagServ) }

// NewDataBinUnbatchedForTest is NewDataBin writing straight through, as every
// DataBin did before its writes were batched: the reference the batched bin is
// compared with.
func NewDataBinUnbatchedForTest(da *DataLayer) DataBin {
	db := NewDataBin(da)
	db.dag = nil
	db.Leaf.Dir = newPlainDirForTest(da)
	return db
}

// NewDataBinFromCidUnbatchedForTest is NewDataBinFromCid writing straight through.
func NewDataBinFromCidUnbatchedForTest(da *DataLayer, c cid.Cid) DataBin {
	return DataBin{DataLayer: da, Leaf: newLeafFromCid(da, da.DagServ, c)}
}

// NewBatchDAGForTest is the batching DAG service a DataBin writes through.
func NewBatchDAGForTest(under ipld.DAGService) *batchDAG { return newBatchDAG(under) }

// FlushForTest writes a batching DAG service's pending nodes.
func FlushForTest(b *batchDAG) error { return b.flush(context.Background()) }
