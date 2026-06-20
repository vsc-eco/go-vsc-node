package wasm_runtime

import (
	"encoding/json"
	"fmt"

	"vsc-node/modules/common/consensusversion"

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

// runtimeMinVersion is the minimum chain-active consensus version at which a
// runtime may be declared on a contract (create_contract / update_contract).
//
// MED #136 (m59 F9 — Contract.Runtime enum has no versioning): the runtime
// string is a consensus input — every node must accept or reject the SAME
// create/update tx identically, or the chain forks. Previously a node simply
// accepted any runtime its binary happened to recognize. The moment a future
// binary adds a third runtime, an owner could declare it: new binaries would
// register the contract (TxResult.Success=true) while old binaries reject it
// (NewFromString(...).IsErr() -> Success=false) on the very same tx -> state
// fork / network split.
//
// Binding each runtime to a minimum consensus version closes that: acceptance
// becomes a pure function of two on-chain, height-addressable inputs — the
// declared runtime string and the chain-active consensus version
// (StateEngine.ActiveConsensusVersion). Every node at a given height resolves
// the identical answer, so old and new binaries never diverge on the same tx.
//
// The two genesis runtimes map to {0,0,0}, so IsSupportedAt is byte-identical
// to the legacy NewFromString check at every height (the floor is always >=
// {0,0,0}). A future runtime MUST be added here with the consensus version
// that introduces it, and that version bumped in consensusversion/version.go,
// in the same commit that wires its execution — mirroring TryCatchICCVersion.
var runtimeMinVersion = map[string]consensusversion.Version{
	AssemblyScript.string: {Major: 0, Consensus: 0, NonConsensus: 0},
	Go.string:             {Major: 0, Consensus: 0, NonConsensus: 0},
}

// IsSupportedAt reports whether the runtime string s may be declared on a
// contract given the chain-active consensus version `active`. It is the
// consensus-gated replacement for NewFromString(s).IsOk() at the create/update
// boundary: a runtime is supported iff this binary recognizes it AND the
// chain-active consensus floor has reached the runtime's minimum version.
//
// Determinism: both `s` (from the tx) and `active` (from the on-chain election
// via ActiveConsensusVersion) are pure functions of on-chain data, so all nodes
// at the same height return the same bool. For the two genesis runtimes the
// min version is {0,0,0} so this returns exactly NewFromString(s).IsOk() — the
// legacy result — at every height. Unknown strings are rejected (same as
// NewFromString), with no panic.
func IsSupportedAt(s string, active consensusversion.Version) bool {
	min, known := runtimeMinVersion[s]
	if !known {
		return false
	}
	return active.MeetsConsensusMin(min)
}

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
		// MED #136 (fix-round-3 C-R24): IsSupportedAt gates every create/update
		// transaction, so this branch is PROVABLY UNREACHABLE for any runtime
		// that passed validation. The generic signature cannot construct a typed
		// result.Err here (Result is constrained to `any`, not result.Result[T]),
		// so we panic instead — which is strictly correct for an impossible state:
		//
		//   - DETERMINISTIC: the unsupported runtime string is on-chain data; all
		//     nodes at the same height would see the same value and take the same
		//     branch, so there is no fork risk.
		//   - NOT a silent success: a zero-value result.Result[T] has err==nil,
		//     i.e. IsOk()==true, which would mask a wiring regression. Panic is
		//     louder and forces investigation.
		//   - UNREACHABLE assertion, NOT a halt risk: the IsSupportedAt gate at
		//     the create/update boundary is the first line of defence; this panic
		//     is the backstop that makes a future registry-wiring regression
		//     immediately visible rather than silently corrupting outputs.
		panic("unsupported runtime: " + r.string)
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
