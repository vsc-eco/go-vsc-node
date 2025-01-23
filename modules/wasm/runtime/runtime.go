package wasm_runtime

import (
	"fmt"

	"github.com/JustinKnueppel/go-result"
)

type runtime struct{ string }

type Runtime = runtime

var (
	AssemblyScript = runtime{"assembly-script"}
	Go             = runtime{"go"}
)

type RuntimeAction[Result any] struct {
	AssemblyScript func() Result
	Go             func() Result
}

func NewFromString(s string) result.Result[Runtime] {
	switch s {
	case AssemblyScript.string:
		return result.Ok(AssemblyScript)
	case Go.string:
		return result.Ok(Go)
	default:
		return result.Err[Runtime](fmt.Errorf("unsupported runtime: %s", s))
	}
}

func (r runtime) String() string {
	return r.string
}

func (r runtime) IsAssemblyScript() bool {
	return r == AssemblyScript
}

func (r runtime) IsGo() bool {
	return r == Go
}

func Execute[Result any](r runtime, action RuntimeAction[Result]) Result {
	switch r {
	case AssemblyScript:
		if action.AssemblyScript == nil {
			var res Result
			return res
		}
		return action.AssemblyScript()
	case Go:
		if action.Go == nil {
			var res Result
			return res
		}
		return action.Go()
	default:
		panic(fmt.Errorf("BUG: unsupported runtime: %s", r))
	}
}
