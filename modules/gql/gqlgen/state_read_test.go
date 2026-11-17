package gqlgen

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"vsc-node/lib/datalayer"

	blockservice "github.com/ipfs/boxo/blockservice"
	blockstore "github.com/ipfs/boxo/blockstore"
	"github.com/ipfs/boxo/exchange/offline"
	"github.com/ipfs/boxo/ipld/merkledag"
	uio "github.com/ipfs/boxo/ipld/unixfs/io"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	dsync "github.com/ipfs/go-datastore/sync"
	format "github.com/ipfs/go-ipld-format"
)

// unreadableUntil is a contract state whose blocks cannot be read for the first
// `failing` loads, as on a node in the moment after a block changed the state.
type unreadableUntil struct {
	format.DAGService
	failing atomic.Int64 // loads of the state that still fail
}

func (d *unreadableUntil) fails() bool { return d.failing.Load() > 0 }

func (d *unreadableUntil) Get(ctx context.Context, c cid.Cid) (format.Node, error) {
	if d.fails() {
		return nil, errors.New("block was not found locally (offline)")
	}
	return d.DAGService.Get(ctx, c)
}

func (d *unreadableUntil) GetMany(ctx context.Context, cids []cid.Cid) <-chan *format.NodeOption {
	out := make(chan *format.NodeOption, len(cids))
	for _, c := range cids {
		n, err := d.Get(ctx, c)
		out <- &format.NodeOption{Node: n, Err: err}
	}
	close(out)
	return out
}

func stateWithKey(t *testing.T) (*unreadableUntil, cid.Cid) {
	t.Helper()
	ctx := context.Background()
	bs := blockstore.NewBlockstore(dsync.MutexWrap(datastore.NewMapDatastore()))
	dag := merkledag.NewDAGService(blockservice.New(bs, offline.Exchange(bs)))
	val := merkledag.NewRawNode([]byte("21"))
	if err := dag.Add(ctx, val); err != nil {
		t.Fatal(err)
	}
	dir := uio.NewDirectory(dag)
	if err := dir.AddChild(ctx, "balance", val); err != nil {
		t.Fatal(err)
	}
	root, err := dir.GetNode()
	if err != nil {
		t.Fatal(err)
	}
	if err := dag.Add(ctx, root); err != nil {
		t.Fatal(err)
	}
	return &unreadableUntil{DAGService: dag}, root.Cid()
}

// readKey is what getStateByKeys does with one key: "" for a key that reads as
// absent.
func readKey(local *datalayer.DataLayer, root cid.Cid, key string) (string, error) {
	bin := datalayer.NewDataBinFromCid(local, root)
	c, err := bin.Get(key)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	node, err := local.DagServ.Get(context.Background(), *c)
	if err != nil {
		return "", err
	}
	return string(node.RawData()), nil
}

// LOCALONLY-EMPTY-1 (testnet 11-16/17): for up to about a second after a block
// changed a contract's state, getStateByKeys answered null for keys that exist
// and dry runs ran on empty state. A request whose state read failed now reads
// again; the caller gets the value, never the empty answer of the failed read.
func TestReadableState_AFailedReadIsReadAgainNotAnsweredAsEmpty(t *testing.T) {
	old := stateReadWaits
	stateReadWaits = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { stateReadWaits = old })
	dag, root := stateWithKey(t)
	da := datalayer.NewOffline(dag)
	for _, failing := range []int64{0, 1, 3, 4} {
		dag.failing.Store(failing)
		reads := 0
		got, err := readableState(context.Background(), da, func(local *datalayer.DataLayer) (string, error) {
			reads++
			v, err := readKey(local, root, "balance")
			dag.failing.Add(-1) // the state becomes readable after `failing` reads
			return v, err
		})
		if err != nil || got != "21" {
			t.Fatalf("unreadable for %d read(s): got %q, %v; want the value 21", failing, got, err)
		}
		if reads != int(failing)+1 {
			t.Fatalf("unreadable for %d read(s): %d reads, want %d", failing, reads, failing+1)
		}
	}
	// a key that is not in a readable state is absent at once, with no retry
	dag.failing.Store(0)
	reads := 0
	got, err := readableState(context.Background(), da, func(local *datalayer.DataLayer) (string, error) {
		reads++
		return readKey(local, root, "no-such-key")
	})
	if err != nil || got != "" || reads != 1 {
		t.Fatalf("absent key of a readable state: %q, %v after %d read(s); want absent after one read", got, err, reads)
	}
}

// If the state stays unreadable the answer is the StateNotReadable error, after
// every wait was used: not null, not a dry run of nothing. Clients match the
// text (the EVM bot holds a vault op for it).
func TestReadableState_StillUnreadableIsAnErrorWithAFixedText(t *testing.T) {
	old := stateReadWaits
	stateReadWaits = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { stateReadWaits = old })
	dag, root := stateWithKey(t)
	dag.failing.Store(1 << 30)
	reads := 0
	got, err := readableState(context.Background(), datalayer.NewOffline(dag), func(local *datalayer.DataLayer) (string, error) {
		reads++
		return readKey(local, root, "balance")
	})
	if err == nil || !strings.HasPrefix(err.Error(), "state not readable right now") || got != "" {
		t.Fatalf("got %q, %v; want the error that starts with %q", got, err, "state not readable right now")
	}
	if StateNotReadable != "state not readable right now" {
		t.Fatalf("StateNotReadable is %q: clients match on the old text", StateNotReadable)
	}
	if reads != len(stateReadWaits)+1 {
		t.Fatalf("%d reads, want %d (one per wait, and the first)", reads, len(stateReadWaits)+1)
	}
	// a request that was cancelled stops waiting
	stateReadWaits = []time.Duration{time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readableState(ctx, datalayer.NewOffline(dag), func(local *datalayer.DataLayer) (string, error) {
		return readKey(local, root, "balance")
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled request: %v, want context.Canceled", err)
	}
}

// The waits cover the longest unreadable window seen on testnet (about 0.9 s).
func TestReadableState_TheWaitsCoverTheMeasuredWindow(t *testing.T) {
	var total time.Duration
	for _, w := range stateReadWaits {
		total += w
	}
	if total < 1200*time.Millisecond || total > 3*time.Second {
		t.Fatalf("the waits add up to %v; they must cover ~0.9 s with room and not hold a request for seconds", total)
	}
}

// A resolver without a data layer (before Init, and the all-nil resolver of
// TestF14) still runs its body once.
func TestReadableState_NoDataLayerRunsTheBodyOnce(t *testing.T) {
	reads := 0
	got, err := readableState(context.Background(), nil, func(local *datalayer.DataLayer) (int, error) {
		reads++
		if local != nil {
			t.Fatal("a view from a nil data layer")
		}
		return 7, nil
	})
	if got != 7 || err != nil || reads != 1 {
		t.Fatalf("got %d, %v after %d run(s)", got, err, reads)
	}
}

// The two resolvers go through readableState, and their bodies read state only
// from the view they are given: one read from the shared local-only view in
// there would be answered as empty again without the request noticing.
func TestReadableState_IsWiredIntoBothResolvers(t *testing.T) {
	res, err := os.ReadFile("schema.resolvers.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range []string{
		"return r.getStateByKeysOn(ctx, contractID, keys, encoding, local)",
		"return r.simulateContractCallsOn(ctx, input, local)",
	} {
		if strings.Count(string(res), call) != 1 {
			t.Errorf("schema.resolvers.go: want exactly one %q", call)
		}
	}
	if n := strings.Count(string(res), "readableState(ctx, r.Da, "); n != 2 {
		t.Errorf("schema.resolvers.go calls readableState %d time(s), want 2", n)
	}
	body, err := os.ReadFile("state_read.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "LocalOnly()") {
		t.Error("state_read.go reads through the shared local-only view; the resolver bodies must read from `local`")
	}
}
