package wasm_runtime

import (
	"encoding/json"
	"fmt"

	"github.com/JustinKnueppel/go-result"
	"go.mongodb.org/mongo-driver/bson"
)

type runtime struct{ string }
type Runtime = runtime

var _ json.Marshaler = runtime{}
var _ json.Unmarshaler = &runtime{}
var _ bson.Marshaler = runtime{}
var _ bson.Unmarshaler = &runtime{}

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

// MarshalJSON implements json.Marshaler.
func (r runtime) MarshalJSON() ([]byte, error) {
	return json.Marshal(r.string)
}

// UnmarshalJSON implements json.Unmarshaler.
func (r *runtime) UnmarshalJSON(data []byte) error {
	return json.Unmarshal(data, &r.string)
}

type serializedBson struct {
	Value string `bson:"value"`
}

// MarshalBSON implements bson.Marshaler.
func (r runtime) MarshalBSON() ([]byte, error) {
	return bson.Marshal(serializedBson{r.string})
}

// UnmarshalBSON implements bson.Unmarshaler.
func (r *runtime) UnmarshalBSON(data []byte) error {
	b := serializedBson{}
	err := bson.Unmarshal(data, &b)
	if err != nil {
		return err
	}
	r.string = b.Value
	return nil
}
