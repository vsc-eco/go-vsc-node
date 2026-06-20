package wasm_context

import (
	wasm_types "vsc-node/modules/wasm/types"

	"github.com/JustinKnueppel/go-result"
)

type contextKey string

const WasmExecCtxKey = contextKey("exec")
const WasmExecCodeCtxKey = contextKey("exec-code")

// WasmInitGuardCtxKey carries a bool into Wasm.Execute that activates the
// `_initialize` hard-fail guard (MED #130 / m57 F-WB-3). The value is the
// chain-active gate decided by the caller (state engine / inter-contract call)
// from ActiveConsensusVersion(blockHeight).MeetsConsensusMin(
// consensusversion.WasmInitGuardVersion). When true, a trapping `_initialize`
// aborts the call with WASM_INIT_ERROR instead of dispatching a handler over an
// uninitialised module. When the key is absent or false (read-only paths that do
// not set it, and every block below the version floor) the host keeps the legacy
// behaviour, so pre-activation execution is byte-identical across binaries.
const WasmInitGuardCtxKey = contextKey("init-guard")

type IOSession interface {
	End() uint
}

// PendulumSwapFeeArgs is the input the pool contract passes to
// system.pendulum_apply_swap_fees. Quantities are base units (int64);
// asset tags are lowercase strings ("hbd", "hive", etc.). The contract
// passes only the swap inputs; the SDK derives gross output, base CLP,
// base protocol fee, and the stabilizer push direction internally on the
// output side — matching the existing pre-pendulum contract math where
// both fee components live in the output asset.
//
// The "exacerbates" hint that older spec drafts asked the contract to
// supply is now derived from the snapshot's s and the swap direction
// (HBD-in raises s, HBD-out lowers s). Letting a contract pass it would
// be a non-determinism vector.
type PendulumSwapFeeArgs struct {
	AssetIn  string
	AssetOut string
	X        int64 // user input, base units
	XReserve int64 // pre-swap input-side reserves
	YReserve int64 // pre-swap output-side reserves
}

// PendulumSwapFeeResult is the SDK method's return shape. The contract
// updates its reserves to (NewXReserve, NewYReserve), pays UserOutput to
// the user, and adds NetworkCreditOutput to its single output-asset
// network-share accumulator. The SDK has already credited
// NodeBucketCreditedHBD to pendulum:nodes:HBD.
type PendulumSwapFeeResult struct {
	UserOutput  int64
	NewXReserve int64
	NewYReserve int64
	// NetworkCreditOutput is the 25% network cut on (totalCLP + totalProtocol),
	// in the output asset of the swap (since both fee components live on the
	// output side under the unified model).
	NetworkCreditOutput   int64
	NodeBucketCreditedHBD int64
	// LpShareOutput is the fee retained for LPs (output units): the LP-kept
	// portions of both the CLP and protocol pots after the network cut. It
	// stays in the pool reserves. NodeShareOutput is the node-runner share
	// (output units) before the HBD conversion, i.e. the node portions of
	// both pots. Together with NetworkCreditOutput they reconcile the fee:
	// LpShareOutput + NodeShareOutput + NetworkCreditOutput == grossOut - UserOutput.
	LpShareOutput   int64
	NodeShareOutput int64
	// MultiplierBps is the stabilizer multiplier m the SDK applied to the base
	// protocol fee, in basis points (10000 = 1.0). SAfterBps is the geometry
	// ratio s = V/E sampled at the snapshot the swap consumed, also in bps.
	MultiplierBps int64
	SAfterBps     int64
}

// AccrueNodeBucketFn is the callback the applier invokes to move the
// node-runner share from the pool contract's HBD ledger account into the
// global `pendulum:nodes` bucket. The execution context constructs this
// closure per-call, binding it to the active LedgerSession so the transfer
// rides on the same rollback unit as the rest of the swap's ledger effects.
//
// Implementations MUST debit the source contract's HBD balance and credit
// the bucket atomically — i.e., a paired (debit, credit) ledger op pair, not
// a unilateral mint. Returning a non-nil error aborts the swap; the wasm
// runtime surfaces the failure to the contract caller.
type AccrueNodeBucketFn func(amountHBD int64) error

// PendulumApplier is the per-call entry point the SDK uses to delegate the
// swap-time fee math + accrual. State engine constructs a concrete
// implementation holding the snapshot DB and whitelist; the per-call ledger
// movement is delegated through the AccrueNodeBucketFn the execution context
// supplies. nil is permitted (e.g. GraphQL read-path simulation) and causes
// the SDK method to return ErrUnimplemented.
type PendulumApplier interface {
	ApplySwapFees(
		contractID, txID string,
		blockHeight uint64,
		args PendulumSwapFeeArgs,
		accrueNodeBucket AccrueNodeBucketFn,
	) result.Result[PendulumSwapFeeResult]
}

type ExecContextValue interface {
	ContractCall(contractId string, method string, payload string, options string) wasm_types.WasmResult
	ContractStateGet(contractId string, key string) result.Result[string]
	// ContractStateGetEx is the presence-disambiguating sibling of
	// ContractStateGet (MED-119). It returns the same value string plus an
	// `exists` bool that is true iff the key is actually present in the target
	// contract's state (an empty stored value => value="" with exists=true; a
	// missing key => value="" with exists=false). The existence bit is derived
	// from the state store's deterministic nil-vs-non-nil result, so every node
	// computes it identically at a given height. Used by the additive
	// contracts.read_ex host function; existing contracts (which call only
	// contracts.read) are unaffected.
	ContractStateGetEx(contractId string, key string) (result.Result[string], bool)
	// SdkErrorDeterminismActive reports whether the v0.2.0 deterministic
	// SDK error-surfacing behaviour is in force for the block this call runs
	// against. It is set once per tx by the state engine from the chain-active
	// consensus version (>= consensusversion.SdkErrorDeterminismVersion) and
	// propagated into nested inter-contract calls, so every node evaluates it
	// identically at a given height. SDK host functions gate their consensus-
	// affecting error rendering on this; false => byte-identical legacy output.
	SdkErrorDeterminismActive() bool
	DeleteEphemState(key string) result.Result[struct{}]
	DeleteState(key string) result.Result[struct{}]
	EnvVar(key string) result.Result[string]
	GetBalance(account string, asset string) int64
	GetEnv() result.Result[string]
	GetEphemState(contractId string, key string) result.Result[string]
	GetState(key string) result.Result[string]
	IOGas() int
	IOSession() IOSession
	Log(msg string)
	PullBalance(from string, amount int64, asset string) result.Result[struct{}]
	Revert()
	SendBalance(to string, amount int64, asset string) result.Result[struct{}]
	SetGasUsage(gasUsed uint)
	SetEphemState(key string, value string) result.Result[struct{}]
	SetState(key string, value string) result.Result[struct{}]
	WithdrawBalance(to string, amount int64, asset string) result.Result[struct{}]
	TssCreateKey(keyId string, keyType string, epochs uint64) result.Result[string]
	TssRenewKey(keyId string, additionalEpochs uint64) result.Result[string]
	TssGetKey(keyId string) result.Result[string]
	TssKeySign(keyId string, msg string) result.Result[string]
	PendulumApplySwapFees(args PendulumSwapFeeArgs) result.Result[PendulumSwapFeeResult]
}
