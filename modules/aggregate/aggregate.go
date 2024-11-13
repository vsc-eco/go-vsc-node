package aggregate

import (
	"context"

	"github.com/chebyrash/promise"
)

type Aggregate struct {
	ctx     context.Context
	cancel  context.CancelFunc
	plugins []Plugin
}

var _ Plugin = &Aggregate{}

func New(plugins []Plugin) *Aggregate {
	ctx, cancel := context.WithCancel(context.Background())
	return &Aggregate{
		ctx,
		cancel,
		plugins,
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
	}
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
