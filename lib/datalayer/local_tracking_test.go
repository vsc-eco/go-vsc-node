package datalayer

import (
	"context"
	"errors"
	"testing"

	blockservice "github.com/ipfs/boxo/blockservice"
	blockstore "github.com/ipfs/boxo/blockstore"
	"github.com/ipfs/boxo/exchange/offline"
	"github.com/ipfs/boxo/ipld/merkledag"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	dsync "github.com/ipfs/go-datastore/sync"
	format "github.com/ipfs/go-ipld-format"
)

// onlyRootDag holds the state's root block and nothing under it: the node has
// the root of a new state but not yet (all of) its shards. That was 46 of the 55
// failed reads in our testnet node's log on 11-16/17.
type onlyRootDag struct {
	format.DAGService
	root cid.Cid
}

func (d onlyRootDag) Get(ctx context.Context, c cid.Cid) (format.Node, error) {
	if c != d.root {
		return nil, errors.New("block was not found locally (offline)")
	}
	return d.DAGService.Get(ctx, c)
}

func (d onlyRootDag) GetMany(ctx context.Context, cids []cid.Cid) <-chan *format.NodeOption {
	out := make(chan *format.NodeOption, len(cids))
	for _, c := range cids {
		n, err := d.Get(ctx, c)
		out <- &format.NodeOption{Node: n, Err: err}
	}
	close(out)
	return out
}

func sharedLocal(dag format.DAGService) *DataLayer {
	l := &DataLayer{DagServ: dag, localOnly: true}
	l.local = l
	return l
}

// LOCALONLY-EMPTY-1 (testnet 11-16/17): a local-only state read that fails is
// answered as an empty directory or an absent key, and the API passed that on
// as "null" and ran dry runs on empty contract state. A request's own tracking
// view records that it happened, for a missing root and for a missing shard
// under a root that is held; the shared view the TSS and oracle readers use
// records nothing and answers as before.
func TestLocalOnlyTracking_RecordsAFailedStateRead(t *testing.T) {
	full, root := lazyTestState(t)
	bs := blockstore.NewBlockstore(dsync.MutexWrap(datastore.NewMapDatastore()))
	nothing := merkledag.NewDAGService(blockservice.New(bs, offline.Exchange(bs)))
	for name, dag := range map[string]format.DAGService{
		"the state's root is not held":       nothing,
		"a shard under the root is not held": onlyRootDag{full, root},
	} {
		shared := sharedLocal(dag)
		view := shared.LocalOnlyTracking()
		if view == shared || !view.localOnly || view.Missed() {
			t.Fatalf("%s: the tracking view must be a new, clean, local-only view", name)
		}
		// GetPathCid answers a missing root as absent and a missing shard as an
		// error; an "absent" from a failed read is always recorded.
		if _, err := GetPathCid(view, root, "k299"); err == nil {
			t.Fatalf("%s: GetPathCid read a key from a state that is not held", name)
		} else if errors.Is(err, osErrNotExist) && !view.Missed() {
			t.Fatalf("%s: the failed key read was answered as absent and not recorded", name)
		}
		view = shared.LocalOnlyTracking()
		// Through a DataBin (what the API and a dry run read with) the state
		// loads as an empty directory: the key reads as absent, and that is
		// recorded.
		bin := NewDataBinFromCid(view, root)
		if _, err := bin.Get("k299"); !errors.Is(err, osErrNotExist) {
			t.Fatalf("%s: DataBin err %v, want the old answer os.ErrNotExist (an empty directory)", name, err)
		}
		if !view.Missed() {
			t.Fatalf("%s: the failed DataBin read was not recorded", name)
		}
		// the shared view answers the same and records nothing
		sharedBin := NewDataBinFromCid(shared, root)
		if _, err := sharedBin.Get("k299"); !errors.Is(err, osErrNotExist) {
			t.Fatalf("%s: shared view err %v, want os.ErrNotExist (an empty directory) as before", name, err)
		}
		if shared.Missed() || shared.missed != nil {
			t.Fatalf("%s: the shared local-only view tracks misses; it must not change", name)
		}
		// and a new request starts clean
		if shared.LocalOnlyTracking().Missed() {
			t.Fatalf("%s: a new tracking view starts with a miss", name)
		}
	}
}

// A key that was never written, read from a state that IS held, is absent and
// is not a miss: a strict reader must not turn healthy reads into errors.
func TestLocalOnlyTracking_AnAbsentKeyOfReadableStateIsNotAMiss(t *testing.T) {
	full, root := lazyTestState(t)
	view := sharedLocal(full).LocalOnlyTracking()
	for _, key := range []string{"nope", "k300", "u/y", "u/x"} {
		if _, err := GetPathCid(view, root, key); !errors.Is(err, osErrNotExist) {
			t.Fatalf("key %q: err %v, want os.ErrNotExist", key, err)
		}
		bin := NewDataBinFromCid(view, root)
		if _, err := bin.Get(key); !errors.Is(err, osErrNotExist) {
			t.Fatalf("key %q through a DataBin: err %v, want os.ErrNotExist", key, err)
		}
	}
	for _, key := range []string{"th", "k0", "k299", "v", "u"} {
		if _, err := GetPathCid(view, root, key); err != nil {
			t.Fatalf("key %q: %v", key, err)
		}
		bin := NewDataBinFromCid(view, root)
		if _, err := bin.Get(key); err != nil {
			t.Fatalf("key %q through a DataBin: %v", key, err)
		}
	}
	if view.Missed() {
		t.Fatal("absent and present keys of a readable state were recorded as a failed read")
	}
}
