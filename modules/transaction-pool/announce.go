package transactionpool

import (
	"context"
	"sort"
	"sync"
	"time"
)

// LOAD-GOSSIP-1, the delivery half. A node announced a transaction it took on
// its API once, at once. Every node handles each gossip message on its own
// goroutine, and a receiver drops an announce whose predecessor nonce it has
// not stored yet. So a burst from one account (nonces n, n+1, ... a few
// milliseconds apart) was checked out of order on the receivers and all but
// the first were dropped, for good: nothing announced them again. On testnet
// 11-17 a burst of 200 left 198 transactions on the node that took them and
// on no other.
//
// The node that takes a transaction on its API now announces it through this
// queue: per account in nonce order, at least announceGap after that account's
// previous announce, so a receiver has stored nonce n before n+1 arrives. And
// while such a transaction is still pending it is announced again: after
// reannounceEvery, then after twice that, and so on (six times within
// reannounceFor), so one lost message is not for ever and a transaction that
// never confirms costs the network a handful of messages, not one a minute.
//
// Only the sender changes. What a node accepts from gossip is untouched, so a
// receiver on any version benefits and no two versions pool different sets.
//
// In memory. After a restart the transactions taken before it are not
// announced again (their signatures are not stored).
const (
	announceGap     = 250 * time.Millisecond
	announceTick    = 50 * time.Millisecond
	reannounceEvery = 60 * time.Second
	reannounceFor   = time.Hour
	// announcePerTick bounds what one tick publishes over all accounts (320 a
	// second), so a sweep that finds many transactions due does not flood the
	// receivers, which drop messages past their own limits.
	announcePerTick = 16
	// announceHeldMax bounds what is kept for announcing again: entries and
	// bytes. Past it a transaction is still announced, once.
	announceHeldMax      = 4096
	announceHeldMaxBytes = 32 << 20
	// announceQueueMax bounds the queue itself; past it add reports false and
	// the caller announces at once, as before.
	announceQueueMax = 2 * announceHeldMax
)

type pendingAnnounce struct {
	id      string
	account string // HashKeyAuths of the required auths
	nonce   uint64
	tx      SerializedVSCTransaction
	first   time.Time // taken on the API
	last    time.Time // last announced (zero: never)
	resends int       // times queued for announcing again
	queued  bool
	held    bool
}

type announcer struct {
	mu        sync.Mutex
	queues    map[string][]*pendingAnnounce // account -> to announce, lowest nonce first
	lastSent  map[string]time.Time          // account -> its last announce
	held      map[string]*pendingAnnounce   // tx id -> kept for announcing again
	heldBytes int
	queuedN   int
	lastSweep time.Time

	// send publishes one announce; pending reports whether the transaction is
	// still unconfirmed in this node's pool (known false: drop it; unknown:
	// keep it).
	send    func(id string, tx SerializedVSCTransaction) error
	pending func(id string) (isPending bool, known bool)
	now     func() time.Time
}

func newAnnouncer(send func(string, SerializedVSCTransaction) error, pending func(string) (bool, bool)) *announcer {
	return &announcer{
		queues:   map[string][]*pendingAnnounce{},
		lastSent: map[string]time.Time{},
		held:     map[string]*pendingAnnounce{},
		send:     send,
		pending:  pending,
		now:      time.Now,
	}
}

// add queues a transaction this node took on its API for its first announce
// and keeps it for announcing again. It reports false when the queue is full:
// the caller then announces it itself.
func (a *announcer) add(id, account string, nonce uint64, tx SerializedVSCTransaction) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if p, ok := a.held[id]; ok { // the same transaction submitted again
		a.enqueueLocked(p)
		return true
	}
	if a.queuedN >= announceQueueMax {
		return false
	}
	p := &pendingAnnounce{id: id, account: account, nonce: nonce, tx: tx, first: a.now()}
	if size := len(tx.Tx) + len(tx.Sig); len(a.held) < announceHeldMax && a.heldBytes+size <= announceHeldMaxBytes {
		a.held[id] = p
		a.heldBytes += size
		p.held = true
	}
	a.enqueueLocked(p)
	return true
}

func (a *announcer) enqueueLocked(p *pendingAnnounce) {
	if p.queued {
		return
	}
	p.queued = true
	a.queuedN++
	q := append(a.queues[p.account], p)
	sort.SliceStable(q, func(i, j int) bool { return q[i].nonce < q[j].nonce })
	a.queues[p.account] = q
}

// step announces what is due: for each account at most its lowest queued nonce,
// and only if announceGap has passed since that account's last announce. It
// returns how many it sent.
func (a *announcer) step() int {
	a.mu.Lock()
	now := a.now()
	var due []*pendingAnnounce
	for account, q := range a.queues {
		if len(due) >= announcePerTick {
			break
		}
		if len(q) == 0 {
			delete(a.queues, account)
			continue
		}
		if last, ok := a.lastSent[account]; ok && now.Sub(last) < announceGap {
			continue
		}
		p := q[0]
		if len(q) == 1 {
			delete(a.queues, account)
		} else {
			a.queues[account] = q[1:]
		}
		p.queued = false
		a.queuedN--
		p.last = now
		a.lastSent[account] = now
		due = append(due, p)
	}
	for account, last := range a.lastSent { // forget idle accounts
		if _, busy := a.queues[account]; !busy && now.Sub(last) > reannounceEvery {
			delete(a.lastSent, account)
		}
	}
	a.mu.Unlock()
	for _, p := range due {
		if err := a.send(p.id, p.tx); err != nil {
			log.Verbose("announcing transaction failed; it is announced again while it is pending", "cid", p.id, "err", err)
		}
	}
	return len(due)
}

// sweep, every reannounceEvery, queues the kept transactions that are still
// pending for another announce and forgets the ones that are not, or that are
// older than reannounceFor.
func (a *announcer) sweep() {
	a.mu.Lock()
	now := a.now()
	if now.Sub(a.lastSweep) < reannounceEvery {
		a.mu.Unlock()
		return
	}
	a.lastSweep = now
	// a copy of what the decision needs: the pool is asked without the lock
	// held, and step writes these fields under it
	type keptTx struct {
		id          string
		first, last time.Time
		resends     int
	}
	kept := make([]keptTx, 0, len(a.held))
	for _, p := range a.held {
		kept = append(kept, keptTx{p.id, p.first, p.last, p.resends})
	}
	a.mu.Unlock()

	drop := map[string]bool{}
	again := map[string]bool{}
	for _, p := range kept {
		if now.Sub(p.first) > reannounceFor {
			drop[p.id] = true
			continue
		}
		isPending, known := a.pending(p.id)
		switch {
		case known && !isPending:
			drop[p.id] = true
		case !p.last.IsZero() && now.Sub(p.last) >= reannounceWait(p.resends):
			again[p.id] = true
		}
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	for id := range drop {
		if p, ok := a.held[id]; ok {
			a.heldBytes -= len(p.tx.Tx) + len(p.tx.Sig)
			p.held = false
			delete(a.held, id)
		}
	}
	for id := range again {
		if p, ok := a.held[id]; ok && !p.queued {
			p.resends++
			a.enqueueLocked(p)
		}
	}
}

// reannounceWait is how long after its last announce a transaction that was
// announced again resends times already is due again: reannounceEvery, doubled
// each time.
func reannounceWait(resends int) time.Duration {
	if resends > 6 {
		resends = 6
	}
	return reannounceEvery << resends
}

// run announces until ctx is done.
func (a *announcer) run(ctx context.Context) {
	t := time.NewTicker(announceTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.sweep()
			a.step()
		}
	}
}
