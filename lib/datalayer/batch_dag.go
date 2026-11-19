package datalayer

import (
	"context"
	"sync"

	"github.com/ipfs/go-cid"
	ipld "github.com/ipfs/go-ipld-format"
)

// batchDAG is the DAG service a DataBin's directories write through. It keeps
// the nodes they add in memory and writes them to the underlying DAG service
// in ONE batch on flush, instead of one write each.
//
// Serializing a state directory adds every shard node it changed, and each
// write to the block store is its own transaction with a synchronous disk
// flush. A slot that changed many shards therefore cost one disk flush per
// shard, which was slow in proportion to the number of new keys. Batched,
// those writes are one transaction.
//
// Nothing else changes: a node's CID is a function of its content, so the
// directory serializes to the same nodes and the same root CID as before. Reads
// see the pending nodes, so a directory reading back a shard it just wrote
// gets it before the flush.
type batchDAG struct {
	ipld.DAGService

	mu      sync.Mutex
	pending map[cid.Cid]ipld.Node
	order   []cid.Cid
}

func newBatchDAG(under ipld.DAGService) *batchDAG {
	return &batchDAG{DAGService: under, pending: make(map[cid.Cid]ipld.Node)}
}

// Add keeps the node until flush.
func (b *batchDAG) Add(_ context.Context, n ipld.Node) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := n.Cid()
	if _, ok := b.pending[c]; !ok {
		b.order = append(b.order, c)
	}
	b.pending[c] = n
	return nil
}

// AddMany keeps the nodes until flush.
func (b *batchDAG) AddMany(ctx context.Context, ns []ipld.Node) error {
	for _, n := range ns {
		if err := b.Add(ctx, n); err != nil {
			return err
		}
	}
	return nil
}

func (b *batchDAG) held(c cid.Cid) (ipld.Node, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, ok := b.pending[c]
	return n, ok
}

// Get answers a pending node from memory, any other from the underlying service.
func (b *batchDAG) Get(ctx context.Context, c cid.Cid) (ipld.Node, error) {
	if n, ok := b.held(c); ok {
		return n, nil
	}
	return b.DAGService.Get(ctx, c)
}

// GetMany answers pending nodes from memory and asks the underlying service for
// the rest.
func (b *batchDAG) GetMany(ctx context.Context, cs []cid.Cid) <-chan *ipld.NodeOption {
	out := make(chan *ipld.NodeOption, len(cs))
	var rest []cid.Cid
	var held []ipld.Node
	for _, c := range cs {
		if n, ok := b.held(c); ok {
			held = append(held, n)
		} else {
			rest = append(rest, c)
		}
	}
	go func() {
		defer close(out)
		for _, n := range held {
			select {
			case out <- &ipld.NodeOption{Node: n}:
			case <-ctx.Done():
				return
			}
		}
		if len(rest) == 0 {
			return
		}
		for opt := range b.DAGService.GetMany(ctx, rest) {
			select {
			case out <- opt:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

// Remove drops a pending node, and the node from the underlying service.
func (b *batchDAG) Remove(ctx context.Context, c cid.Cid) error {
	b.mu.Lock()
	delete(b.pending, c)
	b.mu.Unlock()
	return b.DAGService.Remove(ctx, c)
}

// RemoveMany drops pending nodes, and the nodes from the underlying service.
func (b *batchDAG) RemoveMany(ctx context.Context, cs []cid.Cid) error {
	b.mu.Lock()
	for _, c := range cs {
		delete(b.pending, c)
	}
	b.mu.Unlock()
	return b.DAGService.RemoveMany(ctx, cs)
}

// flush writes every pending node to the underlying service in one batch, in
// the order they were added. The nodes stay pending (and readable from
// memory) until the write has succeeded; after a failed write they are still
// pending, and the next flush tries them again.
func (b *batchDAG) flush(ctx context.Context) error {
	b.mu.Lock()
	nodes := make([]ipld.Node, 0, len(b.order))
	for _, c := range b.order {
		if n, ok := b.pending[c]; ok {
			nodes = append(nodes, n)
		}
	}
	b.mu.Unlock()
	if len(nodes) == 0 {
		return nil
	}
	if err := b.DAGService.AddMany(ctx, nodes); err != nil {
		return err
	}
	b.mu.Lock()
	for _, n := range nodes {
		delete(b.pending, n.Cid())
	}
	kept := b.order[:0]
	for _, c := range b.order {
		if _, ok := b.pending[c]; ok {
			kept = append(kept, c)
		}
	}
	b.order = kept
	b.mu.Unlock()
	return nil
}
