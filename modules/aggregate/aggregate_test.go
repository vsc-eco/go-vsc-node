package aggregate

import (
	"context"
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
// is already dead. Optional name/stopped record the Stop() call for
// shutdown-order assertions.
type blockingPlugin struct {
	name    string
	stopped *[]string
}

func (p blockingPlugin) Init() error { return nil }
func (p blockingPlugin) Start() *promise.Promise[any] {
	return promise.New(func(_ func(any), _ func(error)) {
		select {} // block forever
	})
}
func (p blockingPlugin) Stop() error {
	if p.stopped != nil {
		*p.stopped = append(*p.stopped, p.name)
	}
	return nil
}

// stopAwarePlugin.Start() blocks until Stop() cancels its context and
// then resolves — a pubsub-backed plugin (block-producer/txpool) whose
// service.Close() settles the Start promise.
type stopAwarePlugin struct {
	name    string
	ctx     context.Context
	cancel  context.CancelFunc
	stopped *[]string
}

func newStopAwarePlugin(name string, stopped *[]string) *stopAwarePlugin {
	ctx, cancel := context.WithCancel(context.Background())
	return &stopAwarePlugin{name: name, ctx: ctx, cancel: cancel, stopped: stopped}
}

func (p *stopAwarePlugin) Init() error { return nil }
func (p *stopAwarePlugin) Start() *promise.Promise[any] {
	return promise.New(func(resolve func(any), _ func(error)) {
		<-p.ctx.Done()
		resolve(nil)
	})
}
func (p *stopAwarePlugin) Stop() error {
	*p.stopped = append(*p.stopped, p.name)
	p.cancel()
	return nil
}

// stopRejectingPlugin.Start() blocks until Stop() cancels its context
// and then REJECTS — the streamer pattern, whose Start loops only exit
// on ctx cancel and report that as "exited prematurely".
type stopRejectingPlugin struct {
	name    string
	ctx     context.Context
	cancel  context.CancelFunc
	stopped *[]string
}

func newStopRejectingPlugin(name string, stopped *[]string) *stopRejectingPlugin {
	ctx, cancel := context.WithCancel(context.Background())
	return &stopRejectingPlugin{name: name, ctx: ctx, cancel: cancel, stopped: stopped}
}

func (p *stopRejectingPlugin) Init() error { return nil }
func (p *stopRejectingPlugin) Start() *promise.Promise[any] {
	return promise.New(func(_ func(any), reject func(error)) {
		<-p.ctx.Done()
		reject(errors.New("streamer: block stream: exited prematurely"))
	})
}
func (p *stopRejectingPlugin) Stop() error {
	*p.stopped = append(*p.stopped, p.name)
	p.cancel()
	return nil
}

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

// Graceful shutdown: RequestShutdown must end Run() with nil (exit 0)
// after the full reverse-order teardown, even though plugins reject
// their Start promise when stopped (the streamer pattern) and the last
// plugin never settles on its own. Before RequestShutdown existed,
// nothing could unwind Run's race wait except a plugin failure, which
// Run misreported as a startup error.
func TestRunReturnsNilAfterRequestShutdown(t *testing.T) {
	var stopOrder []string
	agg := New([]Plugin{
		newStopRejectingPlugin("streamer", &stopOrder),   // rejects when stopped
		newStopAwarePlugin("txpool", &stopOrder),         // resolves when stopped
		blockingPlugin{name: "gql", stopped: &stopOrder}, // last plugin, never exits
	})

	errCh := make(chan error, 1)
	go func() { errCh <- agg.Run() }()

	// Wait until Run's Start loop has run — TriggerStart fires right
	// before Run reaches its shutdown race.
	if _, err := agg.Started().Await(context.Background()); err != nil {
		t.Fatalf("aggregate never started: %v", err)
	}

	agg.RequestShutdown()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run returned %v after RequestShutdown, want nil so magid exits 0", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after RequestShutdown — graceful shutdown hangs")
	}

	// Plugins must be stopped in reverse registration order.
	want := []string{"gql", "txpool", "streamer"}
	if len(stopOrder) != len(want) {
		t.Fatalf("stopped %v, want every plugin stopped in order %v", stopOrder, want)
	}
	for i := range want {
		if stopOrder[i] != want[i] {
			t.Fatalf("stop order %v, want %v", stopOrder, want)
		}
	}
}
