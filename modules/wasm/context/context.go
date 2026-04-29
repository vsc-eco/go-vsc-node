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
// computes BaseCLP itself from its own CLP formula and supplies it.
type PendulumSwapFeeArgs struct {
	AssetIn      string
	AssetOut     string
	X            int64 // user input, base units
	XReserve     int64 // pre-swap input-side reserves
	YReserve     int64 // pre-swap output-side reserves
	BaseCLP      int64 // x²·Y / (x+X)² in output asset
	Exacerbates  bool  // contract's hint for stabilizer push
}

// PendulumNetworkCredit returns the per-side amounts the contract should
// add to its internal network-share accounting (claim-based, in pool).
type PendulumNetworkCredit struct {
	AssetInAmount  int64
	AssetOutAmount int64
}

// PendulumSwapFeeResult is the SDK method's return shape. The contract
// updates its reserves to (NewXReserve, NewYReserve), pays UserOutput to
// the user, and adds NetworkCredit to its claim-bucket accounting. The
// SDK has already credited NodeBucketCreditedHBD to pendulum:nodes:HBD.
type PendulumSwapFeeResult struct {
	UserOutput              int64
	NewXReserve             int64
	NewYReserve             int64
	NetworkCredit           PendulumNetworkCredit
	NodeBucketCreditedHBD   int64
	MultiplierQ8            int64
	SAfterQ8                int64
}

// PendulumApplier is the per-call entry point the SDK uses to delegate
// the swap-time fee math + accrual. State engine constructs a concrete
// implementation holding the snapshot DB, whitelist, and ledger system.
// nil is permitted (e.g. in GraphQL read-path simulation) and causes the
// SDK method to return ErrUnimplemented.
type PendulumApplier interface {
	ApplySwapFees(contractID, txID string, blockHeight uint64, args PendulumSwapFeeArgs) result.Result[PendulumSwapFeeResult]
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
