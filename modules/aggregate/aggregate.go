package aggregate

import (
	"context"
	start_status "vsc-node/modules/start-status"

	"github.com/chebyrash/promise"
)

type Aggregate struct {
	ctx         context.Context
	cancel      context.CancelFunc
	plugins     []Plugin
	startStatus start_status.StartStatus
	lastPlugin  *promise.Promise[any]
}

var _ Plugin = &Aggregate{}
var _ start_status.Starter = &Aggregate{}

func New(plugins []Plugin) *Aggregate {
	ctx, cancel := context.WithCancel(context.Background())
	return &Aggregate{
		ctx,
		cancel,
		plugins,
		start_status.New(),
		nil,
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
		_ = a.Stop()
		return err
	}

	if err := a.Stop(); err != nil {
		return err
	}

	_, err := running.Await(a.ctx)

	return err
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
