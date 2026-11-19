package datalayer

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/ipfs/boxo/provider"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	"github.com/ipfs/go-datastore/query"
	badger "github.com/ipfs/go-ds-badger2"
)

// asyncProviderMax is how many CIDs may wait to be handed to the provider
// before new ones are dropped.
const asyncProviderMax = 1 << 18

// asyncProvider hands each stored block's CID to the provider without making
// the writer wait. The providing exchange calls Provide for every block it
// stores, one at a time and while the writer waits, and the provider's queue
// takes one CID at a time from a single worker that writes it to its
// datastore: every block write in the node was paced by that worker, so a slot
// that stored many blocks was slow in proportion to their number.
//
// Announcements are DHT records that let a node with no connection to a holder
// find a block. They never decide state, and connected peers get blocks
// through bitswap without them. So when more than asyncProviderMax CIDs are
// waiting, the newest are dropped (and counted) instead of holding up the node.
// This node never announces a dropped CID again (writing a block it already
// holds does not provide it again, and the provider re-announces nothing on
// its own); other nodes holding the block announce it themselves.
//
// The waiting CIDs are kept in a slice that grows with the backlog, so an idle
// node holds no buffer.
type asyncProvider struct {
	provider.Provider

	mu      sync.Mutex
	queue   []cid.Cid
	dropped uint64
	closed  bool
	wake    chan struct{}
	done    chan struct{}
}

func newAsyncProvider(p provider.Provider) *asyncProvider {
	a := &asyncProvider{Provider: p, wake: make(chan struct{}, 1), done: make(chan struct{})}
	go a.run()
	return a
}

// Provide queues the CID for the provider and returns at once.
func (a *asyncProvider) Provide(_ context.Context, c cid.Cid, _ bool) error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil
	}
	if len(a.queue) >= asyncProviderMax {
		a.dropped++
		n := a.dropped
		a.mu.Unlock()
		if n == 1 || n%10000 == 0 {
			log.Warn("announce backlog full: dropping announcements; the blocks stay stored and served", "waiting", asyncProviderMax, "dropped_so_far", n)
		}
		return nil
	}
	a.queue = append(a.queue, c)
	a.mu.Unlock()
	select {
	case a.wake <- struct{}{}:
	default:
	}
	return nil
}

func (a *asyncProvider) run() {
	for {
		select {
		case <-a.wake:
		case <-a.done:
			return
		}
		for {
			a.mu.Lock()
			if a.closed || len(a.queue) == 0 {
				if len(a.queue) == 0 {
					a.queue = nil // let the backing array go
				}
				a.mu.Unlock()
				break
			}
			c := a.queue[0]
			a.queue = a.queue[1:]
			a.mu.Unlock()
			if err := a.Provider.Provide(context.Background(), c, true); err != nil {
				log.Debug("announce failed", "cid", c, "err", err)
			}
		}
	}
}

// Close stops handing CIDs to the provider; the ones still waiting are dropped.
func (a *asyncProvider) Close() {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}
	a.closed = true
	a.queue = nil
	a.mu.Unlock()
	close(a.done)
}

// openProviderStore opens the provider's queue store: small, written without a
// flush per entry. It holds nothing a node needs to agree with its peers, so a
// store that does not open (after a crash, say) is removed and made again
// instead of keeping the node from starting.
func openProviderStore(path string) (*badger.Datastore, error) {
	opts := badger.DefaultOptions
	opts.SyncWrites = false
	opts.NumMemtables = 2
	opts.MaxTableSize = 2 << 20
	opts.ValueLogFileSize = 16 << 20
	ds, err := badger.NewDatastore(path, &opts)
	if err == nil {
		return ds, nil
	}
	log.Error("announce queue store does not open: removing it and starting an empty one", "path", path, "err", err)
	if rmErr := os.RemoveAll(path); rmErr != nil {
		return nil, fmt.Errorf("announce queue store %s: %v; removing it: %w", path, err, rmErr)
	}
	return badger.NewDatastore(path, &opts)
}

// oldProviderPrefix is where the provider kept its queue in the block store
// before it had a store of its own (boxo provider.DefaultKeyPrefix).
const oldProviderPrefix = "/provider"

// dropOldProviderKeys deletes what the provider left in the block store, in
// batches, in the background. Only keys under /provider/ are touched; blocks
// are under /blocks/.
func dropOldProviderKeys(ds datastore.Batching) {
	ctx := context.Background()
	total := 0
	for {
		res, err := ds.Query(ctx, query.Query{Prefix: oldProviderPrefix, KeysOnly: true, Limit: 10000})
		if err != nil {
			log.Warn("old announce queue cleanup stopped", "err", err)
			return
		}
		entries, err := res.Rest()
		if err != nil {
			log.Warn("old announce queue cleanup stopped", "err", err)
			return
		}
		if len(entries) == 0 {
			if total > 0 {
				log.Info("old announce queue cleanup: removed the provider's old entries from the block store", "entries", total)
			}
			return
		}
		b, err := ds.Batch(ctx)
		if err != nil {
			log.Warn("old announce queue cleanup stopped", "err", err)
			return
		}
		for _, e := range entries {
			k := datastore.NewKey(e.Key)
			if !strings.HasPrefix(k.String(), oldProviderPrefix+"/") {
				log.Error("old announce queue cleanup: refusing a key outside the provider prefix", "key", k.String(), "prefix", oldProviderPrefix+"/")
				return
			}
			if err := b.Delete(ctx, k); err != nil {
				log.Warn("old announce queue cleanup stopped", "err", err)
				return
			}
		}
		if err := b.Commit(ctx); err != nil {
			log.Warn("old announce queue cleanup stopped", "err", err)
			return
		}
		total += len(entries)
	}
}
