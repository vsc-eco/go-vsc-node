package aggregate

import (
	"context"
	"sync/atomic"
	start_status "vsc-node/modules/start-status"

	"github.com/chebyrash/promise"
)

type Aggregate struct {
	ctx         context.Context
	cancel      context.CancelFunc
	plugins     []Plugin
	startStatus start_status.StartStatus
	lastPlugin  *promise.Promise[any]
	// stopRequested records an external graceful-shutdown request
	// (magid's SIGINT/SIGTERM handler). While false, Run keeps its
	// review2 contract: a plugin Start failure ends Run with an error
	// so the supervisor restarts the node. Once requested, teardown is
	// the operator's shutdown, so Run reports the teardown's outcome
	// instead of the cancellation error.
	stopRequested atomic.Bool
}

var _ Plugin = &Aggregate{}
var _ start_status.Starter = &Aggregate{}

func New(plugins []Plugin) *Aggregate {
	ctx, cancel := context.WithCancel(context.Background())
	return &Aggregate{
		ctx:         ctx,
		cancel:      cancel,
		plugins:     plugins,
		startStatus: start_status.New(),
		lastPlugin:  nil,
	}
}

func (a *Aggregate) Run() error {
	if err := a.Init(); err != nil {
		return err
	}

	running := a.Start()

	// review2 HIGH #78: Run previously awaited ONLY a.lastPlugin (the
	// final plugin in the slice — gqlManager). A long-lived plugin
	// such as the Hive streamer rejecting its Start() promise (ingest
	// dead) was not observed until gqlManager itself exited, i.e. at
	// shutdown: the process kept running with dead block ingest and a
	// still-green liveness probe, with no auto-restart.
	//
	// `running` is promise.All over every plugin, which rejects as
	// soon as ANY plugin rejects. Race the intended-shutdown signal
	// (lastPlugin completing) against `running`: a dead plugin now
	// ends Run() immediately, so Run returns the error, magid exits
	// non-zero, and the process supervisor restarts the node instead
	// of masking the failure until shutdown.
	if _, err := promise.Race(a.ctx, a.lastPlugin, running).Await(a.ctx); err != nil {
		// Best-effort orderly shutdown of the surviving plugins; the
		// original error is what callers must see.
		stopErr := a.Stop()
		if a.stopRequested.Load() {
			// Graceful shutdown: the race unwound because
			// RequestShutdown canceled a.ctx. The teardown has run —
			// report its outcome so a clean shutdown exits 0 instead
			// of misreporting context.Canceled (or a stop-time
			// rejection) as a startup failure.
			return stopErr
		}
		return err
	}

	if err := a.Stop(); err != nil {
		return err
	}

	_, err := running.Await(a.ctx)

	return err
}

// RequestShutdown flags a graceful shutdown request and cancels the
// aggregate context. Run() awaits a.ctx at every phase (plugin
// Started() waits in the Start loop, the shutdown race), so the cancel
// unwinds Run to its error branch, where Run performs the full
// reverse-order Stop itself and returns the teardown's outcome (see
// Run). Keeping the teardown on Run's goroutine means Stop can never
// race the Start phase, whatever the signal timing.
func (a *Aggregate) RequestShutdown() {
	a.stopRequested.Store(true)
	a.cancel()
}

// Started implements start_status.Starter.
func (a *Aggregate) Started() *promise.Promise[any] {
	return a.startStatus.Started()
}

// Init implements Plugin.
func (a *Aggregate) Init() error {
	for _, p := range a.plugins {
		if err := p.Init(); err != nil {
			return err
		}
	}
	return nil
}

// Start implements Plugin.
func (a *Aggregate) Start() *promise.Promise[any] {
	promises := make([]*promise.Promise[any], len(a.plugins))
	for i, p := range a.plugins {
		promises[i] = p.Start()
		starter, ok := p.(start_status.Starter)
		if ok {
			starter.Started().Await(a.ctx)
		}
	}
	a.lastPlugin = promises[len(promises)-1]
	a.startStatus.TriggerStart()
	return promise.Then(
		promise.All(a.ctx, promises...),
		a.ctx,
		func([]any) (any, error) {
			return nil, nil
		},
	)
}

// Stop implements Plugin.
func (a *Aggregate) Stop() error {
	for i := len(a.plugins) - 1; i >= 0; i-- {
		if err := a.plugins[i].Stop(); err != nil {
			return err
		}
	}
	return nil
}
