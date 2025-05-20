package start_status

import (
	"sync"
	"sync/atomic"
	"vsc-node/lib/utils"

	"github.com/chebyrash/promise"
)

type startStatus struct {
	started atomic.Bool
	err     atomic.Pointer[error]

	startPromise *promise.Promise[any]

	resolvePromise func()
	rejectPromise  func(error)
}

type StartStatus = *startStatus

type Starter interface {
	Started() *promise.Promise[any]
}

var _ Starter = &startStatus{}

func New() StartStatus {
	s := &startStatus{}
	wg := sync.WaitGroup{}
	wg.Add(1)
	s.startPromise = promise.New(func(resolve func(any), reject func(error)) {
		s.resolvePromise = func() { resolve(nil) }
		s.rejectPromise = reject
		wg.Done()
	})
	wg.Wait()

	return s
}

func (s *startStatus) TriggerStart() {
	s.started.Store(true)
	s.resolvePromise()
}

func (s *startStatus) TriggerStartFailure(err error) {
	s.err.Store(&err)
	s.rejectPromise(err)
}

func (s *startStatus) Started() *promise.Promise[any] {
	if s.started.Load() {
		return utils.PromiseResolve[any](nil)
	}
	if s.err.Load() != nil {
		return utils.PromiseReject[any](*s.err.Load())
	}
	return s.startPromise
}
