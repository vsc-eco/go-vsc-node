package datalayer

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	badger "github.com/ipfs/go-ds-badger2"
	mh "github.com/multiformats/go-multihash"
	"github.com/stretchr/testify/require"
)

type recordingProvider struct {
	mu    sync.Mutex
	got   []cid.Cid
	block chan struct{}
}

func (p *recordingProvider) Provide(_ context.Context, c cid.Cid, _ bool) error {
	if p.block != nil {
		<-p.block
	}
	p.mu.Lock()
	p.got = append(p.got, c)
	p.mu.Unlock()
	return nil
}

func (p *recordingProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.got)
}

func testCid(t *testing.T, i int) cid.Cid {
	h, err := mh.Sum([]byte{byte(i), byte(i >> 8), byte(i >> 16)}, mh.SHA2_256, -1)
	require.NoError(t, err)
	return cid.NewCidV1(cid.Raw, h)
}

// Provide returns at once even while the provider is stuck, everything queued
// reaches the provider in order, and past the bound the newest are dropped.
func TestAsyncProviderNeverHoldsUpTheWriter(t *testing.T) {
	p := &recordingProvider{block: make(chan struct{})}
	a := newAsyncProvider(p)
	defer a.Close()
	start := time.Now() // wall: measures that Provide does not wait
	for i := 0; i < asyncProviderMax+10; i++ {
		require.NoError(t, a.Provide(context.Background(), testCid(t, i), true))
	}
	require.Less(t, time.Since(start), 30*time.Second)
	a.mu.Lock()
	// One CID may already be with the stuck provider.
	waiting, dropped := len(a.queue), a.dropped
	a.mu.Unlock()
	require.GreaterOrEqual(t, waiting, asyncProviderMax-1)
	require.GreaterOrEqual(t, dropped, uint64(9))
	close(p.block)
	require.Eventually(t, func() bool { return p.count() >= asyncProviderMax }, 60*time.Second, 50*time.Millisecond)
	p.mu.Lock()
	require.Equal(t, testCid(t, 0), p.got[0])
	require.Equal(t, testCid(t, 1), p.got[1])
	p.mu.Unlock()
}

func TestAsyncProviderStopsOnClose(t *testing.T) {
	p := &recordingProvider{}
	a := newAsyncProvider(p)
	require.NoError(t, a.Provide(context.Background(), testCid(t, 1), true))
	require.Eventually(t, func() bool { return p.count() == 1 }, 5*time.Second, 10*time.Millisecond)
	a.Close()
	a.Close() // twice is fine
	require.NoError(t, a.Provide(context.Background(), testCid(t, 2), true))
	time.Sleep(100 * time.Millisecond) // wall: nothing may arrive after Close
	require.Equal(t, 1, p.count())
}

// A queue store that does not open is replaced by an empty one.
func TestProviderStoreThatDoesNotOpenIsRemade(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "badger-provider")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "MANIFEST"), []byte("not a badger manifest"), 0o644))
	_, err := badger.NewDatastore(dir, &badger.DefaultOptions)
	require.Error(t, err, "the broken store opens: the case tests nothing")
	ds, err := openProviderStore(dir)
	require.NoError(t, err)
	defer ds.Close()
	require.NoError(t, ds.Put(context.Background(), datastore.NewKey("/x"), []byte("y")))
}

// The cleanup removes the provider's old entries from the block store and
// nothing else: not blocks, not a key that only starts with the same letters.
func TestDropOldProviderKeysTouchesOnlyTheProviderPrefix(t *testing.T) {
	opts := badger.DefaultOptions
	ds, err := badger.NewDatastore(filepath.Join(t.TempDir(), "badger"), &opts)
	require.NoError(t, err)
	defer ds.Close()
	ctx := context.Background()
	keep := []string{"/blocks/CIQABC", "/providers/x", "/provider", "/pins/y"}
	for _, k := range keep {
		require.NoError(t, ds.Put(ctx, datastore.NewKey(k), []byte("keep")))
	}
	b, err := ds.Batch(ctx)
	require.NoError(t, err)
	for i := 0; i < 25000; i++ { // more than one cleanup batch
		require.NoError(t, b.Put(ctx, datastore.NewKey("/provider/queue/"+testCid(t, i).String()), []byte("q")))
	}
	require.NoError(t, b.Commit(ctx))
	require.NoError(t, ds.Put(ctx, datastore.NewKey("/provider/reprovide/lastreprovide"), []byte("t")))
	dropOldProviderKeys(ds)
	for _, k := range keep {
		has, err := ds.Has(ctx, datastore.NewKey(k))
		require.NoError(t, err)
		require.True(t, has, "the cleanup removed %s", k)
	}
	for _, k := range []string{"/provider/queue/" + testCid(t, 7).String(), "/provider/reprovide/lastreprovide"} {
		has, err := ds.Has(ctx, datastore.NewKey(k))
		require.NoError(t, err)
		require.False(t, has, "the cleanup left %s", k)
	}
}
