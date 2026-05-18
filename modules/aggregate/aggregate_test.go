package aggregate

import (
	"errors"
	"testing"
	"time"

	"github.com/chebyrash/promise"
)

// rejectingPlugin.Start() rejects after a short delay (simulates the
// Hive streamer's ingest loop dying).
type rejectingPlugin struct{ err error }

func (p *rejectingPlugin) Init() error { return nil }
func (p *rejectingPlugin) Start() *promise.Promise[any] {
	return promise.New(func(_ func(any), reject func(error)) {
		time.Sleep(20 * time.Millisecond)
		reject(p.err)
	})
}
func (p *rejectingPlugin) Stop() error { return nil }

// blockingPlugin.Start() never resolves until the process exits —
// stands in for gqlManager (the lastPlugin) staying up while ingest
// is already dead.
type blockingPlugin struct{}

func (blockingPlugin) Init() error { return nil }
func (blockingPlugin) Start() *promise.Promise[any] {
	return promise.New(func(_ func(any), _ func(error)) {
		select {} // block forever
	})
}
func (blockingPlugin) Stop() error { return nil }

// review2 HIGH #78: a rejecting non-last plugin must end Run() instead
// of being masked until the last plugin (gqlManager) exits at
// shutdown. Before the fix, Run blocked on a.lastPlugin.Await and this
// test would hang past the timeout.
func TestRunReturnsWhenNonLastPluginRejects(t *testing.T) {
	wantErr := errors.New("streamer: block stream: exited prematurely")
	agg := New([]Plugin{
		&rejectingPlugin{err: wantErr}, // ingest-like plugin, not last
		blockingPlugin{},               // gqlManager-like, last, never exits
	})

	errCh := make(chan error, 1)
	go func() { errCh <- agg.Run() }()

	select {
	case err := <-errCh:
		if !errors.Is(err, wantErr) {
			t.Fatalf("Run returned %v, want the rejecting plugin's error %v", err, wantErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("review2 #78: Run did not return after a non-last plugin rejected — still masked until shutdown")
	}
}
