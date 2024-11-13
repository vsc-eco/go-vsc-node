package aggregate

import "github.com/chebyrash/promise"

type Plugin interface {
	// Runs initialization in order of how they are passed in to `Aggregate`
	Init() error
	// Runs startup and should be non blocking
	Start() *promise.Promise[any]
	// Runs cleanup once the `Aggregate` is finished
	Stop() error
}
