package transactionpool

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type sentAnnounce struct {
	id string
	at time.Duration // since the test's start
}

type announceWorld struct {
	t0      time.Time
	now     time.Time
	sent    []sentAnnounce
	pending map[string]bool // absent: unknown
	sendErr error
	a       *announcer
}

func newAnnounceWorld() *announceWorld {
	w := &announceWorld{t0: time.Unix(1_700_000_000, 0), pending: map[string]bool{}}
	w.now = w.t0
	w.a = newAnnouncer(func(id string, _ SerializedVSCTransaction) error {
		w.sent = append(w.sent, sentAnnounce{id, w.now.Sub(w.t0)})
		return w.sendErr
	}, func(id string) (bool, bool) {
		p, known := w.pending[id]
		return p, known
	})
	w.a.now = func() time.Time { return w.now }
	return w
}

// tick runs the announcer the way run does, every announceTick, for d.
func (w *announceWorld) tick(d time.Duration) {
	for end := w.now.Add(d); w.now.Before(end); {
		w.now = w.now.Add(announceTick)
		w.a.sweep()
		w.a.step()
	}
}

func (w *announceWorld) ids() []string {
	out := make([]string, 0, len(w.sent))
	for _, s := range w.sent {
		out = append(out, s.id)
	}
	return out
}

func tx(n int) SerializedVSCTransaction {
	return SerializedVSCTransaction{Tx: make([]byte, n), Sig: []byte{1}}
}

// LOAD-GOSSIP-1 (testnet 11-17): 200 transactions from two accounts, submitted
// back to back to one node, were announced within milliseconds. The receivers
// handle each announce on its own goroutine and drop one whose predecessor
// nonce is not stored yet: 198 never reached another node. A burst now goes
// out per account in nonce order with announceGap between two announces of the
// same account, whatever order it was taken in; another account is not held up.
func TestAnnounce_ABurstGoesOutInNonceOrderPaced(t *testing.T) {
	w := newAnnounceWorld()
	for _, n := range []uint64{3, 1, 2, 4} { // taken out of order
		if !w.a.add(fmt.Sprintf("a%d", n), "acct-a", n, tx(10)) {
			t.Fatal("add refused")
		}
	}
	w.a.add("b7", "acct-b", 7, tx(10))
	w.tick(4 * announceGap)
	var a []sentAnnounce
	var bAt time.Duration = -1
	for _, s := range w.sent {
		if s.id == "b7" {
			bAt = s.at
		} else {
			a = append(a, s)
		}
	}
	if len(a) != 4 || a[0].id != "a1" || a[1].id != "a2" || a[2].id != "a3" || a[3].id != "a4" {
		t.Fatalf("account a announced %v, want a1 a2 a3 a4 in nonce order", w.ids())
	}
	for i := 1; i < len(a); i++ {
		if gap := a[i].at - a[i-1].at; gap < announceGap {
			t.Fatalf("%s followed %s after %v, want at least %v", a[i].id, a[i-1].id, gap, announceGap)
		}
	}
	if bAt != a[0].at {
		t.Fatalf("the other account's transaction went out at %v, want with the first tick (%v): it must not wait for account a", bAt, a[0].at)
	}
}

// One lost message is not for ever: while a transaction this node took is still
// pending it is announced again, after reannounceEvery and then after twice the
// previous wait each time. Once it is confirmed (or gone) it is forgotten.
func TestAnnounce_APendingTransactionIsAnnouncedAgainUntilItIsNot(t *testing.T) {
	w := newAnnounceWorld()
	w.pending["x"] = true
	w.a.add("x", "acct-a", 1, tx(10))
	w.tick(reannounceEvery - time.Second)
	if len(w.sent) != 1 {
		t.Fatalf("%d announces in the first %v, want 1", len(w.sent), reannounceEvery-time.Second)
	}
	// again after 1 minute, then 2 minutes after that, then 4
	for i, at := range []time.Duration{reannounceEvery, 3 * reannounceEvery, 7 * reannounceEvery} {
		w.tick(w.t0.Add(at).Sub(w.now) - time.Second)
		if len(w.sent) != i+1 {
			t.Fatalf("%d announces just before %v, want %d", len(w.sent), at, i+1)
		}
		w.tick(3 * time.Second)
		if len(w.sent) != i+2 {
			t.Fatalf("%d announces just after %v, want %d (announced again while pending)", len(w.sent), at, i+2)
		}
	}
	w.pending["x"] = false // confirmed
	w.tick(20 * reannounceEvery)
	if len(w.sent) != 4 || len(w.a.held) != 0 || w.a.heldBytes != 0 {
		t.Fatalf("after it was confirmed: %d announces, %d kept, %d bytes; want 4, 0, 0", len(w.sent), len(w.a.held), w.a.heldBytes)
	}
}

// A pool read that fails says nothing about the transaction: it is kept and
// announced again. After reannounceFor it is given up whatever the pool says.
func TestAnnounce_KeptWhenThePoolCannotBeReadAndDroppedAfterAnHour(t *testing.T) {
	w := newAnnounceWorld() // "x" absent from pending: unknown
	w.a.add("x", "acct-a", 1, tx(10))
	w.tick(7*reannounceEvery + 2*time.Second)
	if len(w.sent) < 3 || len(w.a.held) != 1 {
		t.Fatalf("pool unreadable: %d announces, %d kept; want it kept and announced again", len(w.sent), len(w.a.held))
	}
	w.tick(reannounceFor)
	if len(w.a.held) != 0 {
		t.Fatalf("still kept %v after it was taken", w.now.Sub(w.t0))
	}
	n := len(w.sent)
	w.tick(3 * reannounceEvery)
	if len(w.sent) != n {
		t.Fatalf("announced %d more times after it was dropped", len(w.sent)-n)
	}
}

// A publish that fails does not lose the transaction: it stays kept and goes
// out again at the next round.
func TestAnnounce_AFailedPublishIsTriedAgain(t *testing.T) {
	w := newAnnounceWorld()
	w.pending["x"] = true
	w.sendErr = errors.New("no peers")
	w.a.add("x", "acct-a", 1, tx(10))
	w.tick(time.Second)
	w.sendErr = nil
	w.tick(reannounceEvery + time.Second)
	if len(w.sent) != 2 {
		t.Fatalf("%d publish attempts, want 2 (the failed one and the next round)", len(w.sent))
	}
}

// What is kept for announcing again is bounded; past the bound a transaction
// is still announced once. Past the queue's own bound add reports false and the
// caller announces at once, as before the queue.
func TestAnnounce_Bounds(t *testing.T) {
	w := newAnnounceWorld()
	for i := 0; i < announceHeldMax+10; i++ {
		if !w.a.add(fmt.Sprintf("t%d", i), fmt.Sprintf("acct-%d", i), 1, tx(10)) {
			t.Fatalf("add %d refused below the queue bound", i)
		}
	}
	if len(w.a.held) != announceHeldMax {
		t.Fatalf("%d kept, want the bound %d", len(w.a.held), announceHeldMax)
	}
	w.tick(announceTick)
	if len(w.sent) != announcePerTick {
		t.Fatalf("%d announced in one tick, want the per-tick bound %d", len(w.sent), announcePerTick)
	}
	w.tick(time.Duration((announceHeldMax+10)/announcePerTick+1) * announceTick)
	if len(w.sent) != announceHeldMax+10 {
		t.Fatalf("%d announced, want all %d (the ones past the kept bound once)", len(w.sent), announceHeldMax+10)
	}
	w = newAnnounceWorld()
	big := tx(announceHeldMaxBytes - 1) // with its 1-byte signature: exactly the bound
	w.a.add("big", "acct-a", 1, big)
	w.a.add("next", "acct-b", 1, tx(10))
	if len(w.a.held) != 1 || w.a.held["big"] == nil {
		t.Fatalf("kept %d, want only the first: the byte bound is used up", len(w.a.held))
	}
	w = newAnnounceWorld()
	for i := 0; i < announceQueueMax; i++ {
		w.a.add(fmt.Sprintf("t%d", i), "acct-a", uint64(i), tx(1))
	}
	if w.a.add("over", "acct-a", uint64(announceQueueMax), tx(1)) {
		t.Fatal("add accepted past the queue bound; the caller must announce it itself")
	}
}

// The pool New builds has the queue; IngestTx hands API transactions to it.
func TestAnnounce_ThePoolHasTheQueue(t *testing.T) {
	tp := New(nil, nil, nil, nil, nil, nil, nil, nil, nil, "vsc-testnet")
	if tp.announcer == nil {
		t.Fatal("New built a pool without the announce queue")
	}
}

// IngestTx hands the transaction to the queue and Start runs it. (A source
// check: an IngestTx that gets that far needs a signed transaction and the
// databases; the devnet burst test covers it end to end.)
func TestAnnounce_IsWiredIntoThePool(t *testing.T) {
	src, err := os.ReadFile("transaction-pool.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{
		"tp.announcer.add(cidz.String(), hashAuths, txShell.Headers.Nonce, sTx)",
		"go tp.announcer.run(tp.service.Context())",
		"tp.announcer = newAnnouncer(tp.Broadcast, tp.stillPending)",
	} {
		if strings.Count(string(src), text) != 1 {
			t.Errorf("transaction-pool.go: want exactly one %q", text)
		}
	}
}

// The queue is used from the API goroutines (add) and its own loop (step,
// sweep) at once: nothing is lost and nothing is announced twice. Run under
// -race.
func TestAnnounce_ConcurrentUse(t *testing.T) {
	const accounts, each = 8, 200
	var mu sync.Mutex
	seen := map[string]int{}
	a := newAnnouncer(func(id string, _ SerializedVSCTransaction) error {
		mu.Lock()
		seen[id]++
		mu.Unlock()
		return nil
	}, func(string) (bool, bool) { return true, true })
	var clock atomic.Int64 // nanoseconds since the start
	a.now = func() time.Time { return time.Unix(1_700_000_000, 0).Add(time.Duration(clock.Load())) }
	stop := make(chan struct{})
	stepping := make(chan struct{})
	go func() { // the pool's loop, while the API goroutines add
		defer close(stepping)
		for {
			select {
			case <-stop:
				return
			default:
				// time moves, but never as far as the first announce-again
				// (reannounceEvery), however long the adds take on a busy machine
				if clock.Load() < int64(reannounceEvery/2) {
					clock.Add(int64(time.Microsecond))
				}
				a.sweep()
				a.step()
			}
		}
	}()
	var wg sync.WaitGroup
	for g := 0; g < accounts; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if !a.add(fmt.Sprintf("g%d-%d", g, i), fmt.Sprintf("acct-%d", g), uint64(i), tx(8)) {
					t.Errorf("add g%d-%d refused", g, i)
				}
			}
		}(g)
	}
	wg.Wait()
	close(stop)
	<-stepping
	// drain what is still queued (no sweep: nothing is announced again here)
	for i := 0; i < each+10; i++ {
		clock.Add(int64(announceGap))
		a.step()
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != accounts*each {
		t.Fatalf("%d of %d transactions announced", len(seen), accounts*each)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("%s announced %d times, want once", id, n)
		}
	}
}
