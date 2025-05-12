package start_status

import (
	"sync/atomic"
	"vsc-node/lib/utils"

	"github.com/chebyrash/promise"
)

type startStatus struct {
	started        atomic.Bool
	startPromise   *promise.Promise[any]
	resolvePromise func()
}

type StartStatus = *startStatus

type Starter interface {
	Started() *promise.Promise[any]
}

var _ Starter = &startStatus{}

func New() StartStatus {
	s := &startStatus{}
	s.startPromise = promise.New(func(resolve func(any), reject func(error)) {
		s.resolvePromise = func() { resolve(nil) }
	})

	return s
}

func (s *startStatus) TriggerStart() {
	s.started.Store(true)
	s.resolvePromise()
}

func (s *startStatus) Started() *promise.Promise[any] {
	if s.started.Load() {
		return utils.PromiseResolve[any](nil)
	}
	return s.startPromise
}
