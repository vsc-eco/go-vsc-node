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
	}
}

func (a *Aggregate) Run() error {
	if err := a.Init(); err != nil {
		return err
	}

	if _, err := a.Start().Await(a.ctx); err != nil {
		return err
	}

	if err := a.Stop(); err != nil {
		return err
	}

	return nil
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
	for _, p := range a.plugins {
		if err := p.Stop(); err != nil {
			return err
		}
	}
	return nil
}
