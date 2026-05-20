package gateway

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	systemconfig "vsc-node/modules/common/system-config"

	"github.com/vsc-eco/hivego"
)

// TestMsgChanConcurrentAccessNoRace exercises the msgChan helpers the way the
// running node does: the Tick* goroutines create/delete entries while the
// pubsub dispatch goroutine looks one up and sends. Before the mutex guard the
// unsynchronized map access aborted the process with "fatal error: concurrent
// map writes"; under `go test -race` this test trips the detector without it.
func TestMsgChanConcurrentAccessNoRace(t *testing.T) {
	ms := &MultiSig{msgChan: make(map[string]chan *p2pMessage)}
	keys := []string{"a", "b", "c", "d"}

	const workers = 8
	const iters = 500
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				k := keys[(w+i)%len(keys)]
				switch i % 3 {
				case 0:
					ms.setMsgChan(k)
				case 1:
					// mirrors p2p.go HandleMessage: guarded read + non-blocking send
					if ch := ms.getMsgChan(k); ch != nil {
						select {
						case ch <- &p2pMessage{Type: "sign_response"}:
						default:
						}
					}
				case 2:
					ms.deleteMsgChan(k)
				}
			}
		}(w)
	}
	wg.Wait()
}

type okAcctClient struct{}

func (okAcctClient) GetAccount(_ []string) ([]hivego.AccountData, error) {
	// WeightThreshold > 0 so waitForSigs proceeds into its collection loop
	// instead of aborting on the threshold==0 guard.
	return []hivego.AccountData{{Owner: hivego.Authority{WeightThreshold: 1}}}, nil
}

// TestWaitForSigsNoGoroutineLeakOnTimeout drives waitForSigs to its collection
// loop and lets it time out many times. Before the fix the inner collector
// goroutine blocked forever on a receive from a deleted/nil channel, leaking
// one goroutine (and its closure) per timeout. After the fix it selects on
// ctx.Done() and exits, so the goroutine count returns to baseline.
func TestWaitForSigsNoGoroutineLeakOnTimeout(t *testing.T) {
	ms := &MultiSig{
		sconf:         systemconfig.MocknetConfig(),
		accountClient: okAcctClient{},
		msgChan:       make(map[string]chan *p2pMessage),
	}

	tx := hivego.HiveTransaction{
		Expiration: time.Now().Add(time.Minute).UTC().Format("2006-01-02T15:04:05"),
	}
	txId, err := tx.GenerateTrxId()
	if err != nil {
		t.Fatalf("GenerateTrxId: %v", err)
	}

	runtime.GC()
	before := runtime.NumGoroutine()

	const n = 200
	for i := 0; i < n; i++ {
		ms.setMsgChan(txId) // the real Tick* path registers the channel first
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
		_, _, _ = ms.waitForSigs(ctx, tx, txId)
		cancel()
		ms.deleteMsgChan(txId)
	}

	// Allow the collectors to unwind.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before+5 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if leaked := runtime.NumGoroutine() - before; leaked > 5 {
		t.Fatalf("waitForSigs leaked goroutines across %d timeouts: +%d", n, leaked)
	}
}
