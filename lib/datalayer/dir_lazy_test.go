package datalayer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"

	"github.com/ipfs/boxo/blockservice"
	blockstore "github.com/ipfs/boxo/blockstore"
	"github.com/ipfs/boxo/exchange/offline"
	"github.com/ipfs/boxo/ipld/merkledag"
	uio "github.com/ipfs/boxo/ipld/unixfs/io"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	dsync "github.com/ipfs/go-datastore/sync"
	format "github.com/ipfs/go-ipld-format"
)

// lazyTestState builds a contract-state-like directory: raw values at the top,
// a nested directory, and enough top-level entries to shard into a HAMT.
func lazyTestState(t *testing.T) (format.DAGService, cid.Cid) {
	t.Helper()
	ctx := context.Background()
	bs := blockstore.NewBlockstore(dsync.MutexWrap(datastore.NewMapDatastore()))
	dag := merkledag.NewDAGService(blockservice.New(bs, offline.Exchange(bs)))
	uio.HAMTShardingSize = 256
	raw := func(s string) format.Node {
		n := merkledag.NewRawNode([]byte(s))
		if err := dag.Add(ctx, n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	sub := uio.NewDirectory(dag)
	_ = sub.AddChild(ctx, "x", raw("sub-x"))
	subNode, err := sub.GetNode()
	if err != nil {
		t.Fatal(err)
	}
	if err := dag.Add(ctx, subNode); err != nil {
		t.Fatal(err)
	}
	root := uio.NewDirectory(dag)
	_ = root.AddChild(ctx, "th", raw("1"))
	_ = root.AddChild(ctx, "v", raw("registry"))
	_ = root.AddChild(ctx, "u", subNode)
	for i := 0; i < 300; i++ {
		_ = root.AddChild(ctx, fmt.Sprintf("k%d", i), raw(fmt.Sprintf("val%d", i)))
	}
	rootNode, err := root.GetNode()
	if err != nil {
		t.Fatal(err)
	}
	if err := dag.Add(ctx, rootNode); err != nil {
		t.Fatal(err)
	}
	if n := len(rootNode.Links()); n >= 303 {
		t.Fatalf("fixture: the root has %d direct links; it did not shard into a HAMT", n)
	}
	return dag, rootNode.Cid()
}

var lazyTestPaths = []string{"th", "v", "u", "u/x", "u/y", "nope", "th/x", "k0", "k5", "k299", "k300", "u/", "u//x", "", "/", "u/x/y"}

// 18u: GetPathCid answers every path exactly as DataBin.Get does.
func TestGetPathCidMatchesDataBinGet(t *testing.T) {
	dag, root := lazyTestState(t)
	da := &DataLayer{DagServ: dag}
	bin := NewDataBinFromCid(da, root)
	if _, err := bin.Get("k5"); err != nil {
		t.Fatalf("fixture: DataBin cannot read k5: %v", err)
	}
	for _, p := range lazyTestPaths {
		want, werr := bin.Get(p)
		got, gerr := GetPathCid(da, root, p)
		t.Logf("path %-8q DataBin(%v,%v) GetPathCid(%v,%v)", p, want, werr, got, gerr)
		if (werr == nil) != (gerr == nil) {
			t.Errorf("path %q: DataBin err %v, GetPathCid err %v", p, werr, gerr)
			continue
		}
		if werr == nil && !want.Equals(*got) {
			t.Errorf("path %q: DataBin %s, GetPathCid %s", p, want, got)
		}
	}
}

// LocalOnly: a state not held locally reads as absent, as the LocalOnly
// DataBin's empty directory does.
func TestGetPathCidLocalOnlyMissingStateIsAbsent(t *testing.T) {
	_, root := lazyTestState(t)
	bs := blockstore.NewBlockstore(dsync.MutexWrap(datastore.NewMapDatastore()))
	empty := merkledag.NewDAGService(blockservice.New(bs, offline.Exchange(bs)))
	local := &DataLayer{DagServ: empty, localOnly: true}
	if _, err := GetPathCid(local, root, "th"); !errors.Is(err, errNotExistForTest()) {
		t.Fatalf("err %v, want os.ErrNotExist", err)
	}
	localBin := NewDataBinFromCid(local, root)
	if _, err := localBin.Get("th"); err == nil {
		t.Fatal("fixture: the LocalOnly DataBin read a state it does not hold")
	}
}

type flakyDag struct {
	format.DAGService
	failures int64
	gets     atomic.Int64
}

func (f *flakyDag) Get(ctx context.Context, c cid.Cid) (format.Node, error) {
	if f.gets.Add(1) <= f.failures {
		return nil, errors.New("injected blockstore fault")
	}
	return f.DAGService.Get(ctx, c)
}

// On the full data layer a read fault is retried, never read as "absent"
// (fail-stop, as newLeafFromCid).
func TestGetPathCidRetriesReadFaults(t *testing.T) {
	dag, root := lazyTestState(t)
	flaky := &flakyDag{DAGService: dag, failures: 2}
	got, err := GetPathCid(&DataLayer{DagServ: flaky}, root, "th")
	if err != nil {
		t.Fatalf("a transient fault was returned as %v", err)
	}
	fullBin := NewDataBinFromCid(&DataLayer{DagServ: dag}, root)
	want, _ := fullBin.Get("th")
	if !want.Equals(*got) {
		t.Fatalf("got %s, want %s", got, want)
	}
	if flaky.gets.Load() <= 2 {
		t.Fatal("the fault path was not exercised")
	}
}

func errNotExistForTest() error { return osErrNotExist }

var osErrNotExist = os.ErrNotExist

// A value present as a link but unreadable is an error, not an absence, for
// both readers (an unreadable value fails closed), and it is not retried (the
// DataBin does not retry a value fetch either).
func TestGetPathCidUnreadableValueIsAnErrorNotAbsent(t *testing.T) {
	ctx := context.Background()
	bs := blockstore.NewBlockstore(dsync.MutexWrap(datastore.NewMapDatastore()))
	dag := merkledag.NewDAGService(blockservice.New(bs, offline.Exchange(bs)))
	ghost := merkledag.NewRawNode([]byte("never stored")) // linked, block not in the store
	root := uio.NewDirectory(dag)
	if err := root.AddChild(ctx, "v", ghost); err != nil {
		t.Fatal(err)
	}
	rootNode, err := root.GetNode()
	if err != nil {
		t.Fatal(err)
	}
	if err := dag.Add(ctx, rootNode); err != nil {
		t.Fatal(err)
	}
	da := &DataLayer{DagServ: dag}
	bin := NewDataBinFromCid(da, rootNode.Cid())
	_, werr := bin.Get("v")
	_, gerr := GetPathCid(da, rootNode.Cid(), "v")
	if werr == nil || errors.Is(werr, os.ErrNotExist) {
		t.Fatalf("fixture: DataBin.Get on an unreadable value returned %v", werr)
	}
	if gerr == nil || errors.Is(gerr, os.ErrNotExist) {
		t.Fatalf("GetPathCid on an unreadable value returned %v, want a read error like DataBin's (%v)", gerr, werr)
	}
	local := &DataLayer{DagServ: dag, localOnly: true}
	if _, lerr := GetPathCid(local, rootNode.Cid(), "v"); lerr == nil || errors.Is(lerr, os.ErrNotExist) {
		t.Fatalf("LocalOnly: an unreadable value read as %v, want a read error (fail closed)", lerr)
	}
}
