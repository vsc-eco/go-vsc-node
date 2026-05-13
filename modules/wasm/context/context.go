package wasm_context

import (
	wasm_types "vsc-node/modules/wasm/types"

	"github.com/JustinKnueppel/go-result"
)

type contextKey string

const WasmExecCtxKey = contextKey("exec")
const WasmExecCodeCtxKey = contextKey("exec-code")

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
	UserOutput            int64
	NewXReserve           int64
	NewYReserve           int64
	// NetworkCreditOutput is the 25% network cut on (totalCLP + totalProtocol),
	// in the output asset of the swap (since both fee components live on the
	// output side under the unified model).
	NetworkCreditOutput   int64
	NodeBucketCreditedHBD int64
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
