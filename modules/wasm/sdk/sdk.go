package sdk

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"vsc-node/lib/dids"
	"vsc-node/modules/common/params"
	"vsc-node/modules/db/vsc/contracts"
	tss_db "vsc-node/modules/db/vsc/tss"
	wasm_context "vsc-node/modules/wasm/context"

	wasm_types "vsc-node/modules/wasm/types"

	ethCrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
	bls "github.com/protolambda/bls12-381-util"

	"github.com/JustinKnueppel/go-result"
)

type SdkResultStruct = wasm_types.WasmResultStruct
type SdkResult = result.Result[SdkResultStruct]

var (
	ErrInvalidArgument = result.Err[SdkResultStruct](
		errors.Join(fmt.Errorf(contracts.SDK_ERROR), fmt.Errorf("invalid argument")),
	)
	ErrUnimplemented = result.Err[SdkResultStruct](
		errors.Join(fmt.Errorf(contracts.SDK_ERROR), fmt.Errorf("unimplemented")),
	)
)

type sdkFunc any

// sdkNamespacesRef is set by init() so that system.call can look up functions
// without creating an initialization cycle (SdkNamespaces → Resolve → SdkNamespaces).
var sdkNamespacesRef *map[string]map[string]sdkFunc

// onMainnet is set once at startup via Init and controls which Bitcoin address
// prefix is valid in verify_address.
var onMainnet bool

// Init configures network-dependent SDK behaviour. Call this once at startup,
// before any contract execution, passing sysConfig.OnMainnet().
func Init(mainnet bool) {
	onMainnet = mainnet
}

func init() {
	sdkNamespacesRef = &SdkNamespaces
}

// Resolve looks up an SDK function by its full "namespace.method" name.
func Resolve(name string) (sdkFunc, bool) {
	ns, method, ok := strings.Cut(name, ".")
	if !ok {
		return nil, false
	}
	methods, ok := SdkNamespaces[ns]
	if !ok {
		return nil, false
	}
	fn, ok := methods[method]
	return fn, ok
}

// SdkNamespaces groups SDK functions by namespace. The full import name seen by
// contracts is "namespace.method", e.g. "tss.create_key" or "tss_v2.create_key".
// Add a new versioned namespace (e.g. "tss_v2") to introduce breaking changes
// while keeping old namespaces intact for deployed contracts.
var SdkNamespaces = map[string]map[string]sdkFunc{

	// -------------------------------------------------------------------------
	// console
	// -------------------------------------------------------------------------
	"console": {
		"log": func(ctx context.Context, a any) SdkResult {
			eCtx := ctx.Value(wasm_context.WasmExecCtxKey).(wasm_context.ExecContextValue)
			s, ok := a.(string)
			if !ok {
				return ErrInvalidArgument
			}
			session := eCtx.IOSession()
			eCtx.Log(s)
			gas := session.End()
			return result.Ok(SdkResultStruct{Gas: gas})
		},
	},

	// -------------------------------------------------------------------------
	// db — persistent contract state
	// -------------------------------------------------------------------------
	"db": {
		"set_object": func(ctx context.Context, arg1 any, arg2 any) SdkResult {
			eCtx := ctx.Value(wasm_context.WasmExecCtxKey).(wasm_context.ExecContextValue)
			key, ok := arg1.(string)
			if !ok {
				return ErrInvalidArgument
			}
			val, ok := arg2.(string)
			if !ok {
				return ErrInvalidArgument
			}
			session := eCtx.IOSession()
			return result.Map(
				eCtx.SetState(key, val),
				func(struct{}) SdkResultStruct { return SdkResultStruct{Gas: session.End()} },
			)
		},
		"get_object": func(ctx context.Context, a any) SdkResult {
			eCtx := ctx.Value(wasm_context.WasmExecCtxKey).(wasm_context.ExecContextValue)
			key, ok := a.(string)
			if !ok {
				return ErrInvalidArgument
			}
			session := eCtx.IOSession()
			return result.Map(
				eCtx.GetState(key),
				func(s string) SdkResultStruct { return SdkResultStruct{Result: s, Gas: session.End()} },
			)
		},
		"rm_object": func(ctx context.Context, a any) SdkResult {
			eCtx := ctx.Value(wasm_context.WasmExecCtxKey).(wasm_context.ExecContextValue)
			key, ok := a.(string)
			if !ok {
				return ErrInvalidArgument
			}
			session := eCtx.IOSession()
			return result.Map(
				eCtx.DeleteState(key),
				func(struct{}) SdkResultStruct { return SdkResultStruct{Gas: session.End()} },
			)
		},
	},

	// -------------------------------------------------------------------------
	// ephem_db — ephemeral (non-persistent) contract state
	// -------------------------------------------------------------------------
	"ephem_db": {
		"get_object": func(ctx context.Context, arg1 any, arg2 any) SdkResult {
			eCtx := ctx.Value(wasm_context.WasmExecCtxKey).(wasm_context.ExecContextValue)
			contractId, ok := arg1.(string)
			if !ok {
				return ErrInvalidArgument
			}
			key, ok := arg2.(string)
			if !ok {
				return ErrInvalidArgument
			}
			return result.Map(
				eCtx.GetEphemState(contractId, key),
				func(s string) SdkResultStruct {
					return SdkResultStruct{
						Result: s,
						Gas:    uint(params.EPHEM_IO_GAS * (len(key) + len(s))),
					}
				},
			)
		},
		"set_object": func(ctx context.Context, arg1 any, arg2 any) SdkResult {
			eCtx := ctx.Value(wasm_context.WasmExecCtxKey).(wasm_context.ExecContextValue)
			key, ok := arg1.(string)
			if !ok {
				return ErrInvalidArgument
			}
			val, ok := arg2.(string)
			if !ok {
				return ErrInvalidArgument
			}
			return result.Map(
				eCtx.SetEphemState(key, val),
				func(struct{}) SdkResultStruct {
					return SdkResultStruct{Gas: uint(params.EPHEM_IO_GAS * (len(key) + len(val)))}
				},
			)
		},
		"rm_object": func(ctx context.Context, a any) SdkResult {
			eCtx := ctx.Value(wasm_context.WasmExecCtxKey).(wasm_context.ExecContextValue)
			key, ok := a.(string)
			if !ok {
				return ErrInvalidArgument
			}
			return result.Map(
				eCtx.DeleteEphemState(key),
				func(struct{}) SdkResultStruct {
					return SdkResultStruct{Gas: uint(params.EPHEM_IO_GAS * len(key))}
				},
			)
		},
	},

	// -------------------------------------------------------------------------
	// system
	// -------------------------------------------------------------------------
	"system": {
		"call": func(ctx context.Context, arg1 any, arg2 any) SdkResult {
			callArg, ok := arg1.(string)
			if !ok {
				return ErrInvalidArgument
			}
			rawValArg, ok := arg2.(string)
			if !ok {
				return ErrInvalidArgument
			}
			var valArg struct {
				Arg0 string `json:"arg0"`
			}
			if err := json.Unmarshal([]byte(rawValArg), &valArg); err != nil {
				return result.Err[SdkResultStruct](
					errors.Join(fmt.Errorf(contracts.SDK_ERROR), fmt.Errorf("INVALID_ARGUMENT_JSON"), err),
				)
			}

			// Use sdkNamespacesRef (set in init()) to avoid an init cycle.
			if ns, method, hasDot := strings.Cut(callArg, "."); hasDot {
				if methods, ok2 := (*sdkNamespacesRef)[ns]; ok2 {
					if f, ok3 := methods[method]; ok3 {
						fn, ok4 := f.(func(context.Context, any) SdkResult)
						if !ok4 {
							return result.Err[SdkResultStruct](
								errors.Join(fmt.Errorf(contracts.SDK_ERROR), fmt.Errorf("ARITY_MISMATCH")),
							)
						}
						return func() (ret SdkResult) {
							defer func() {
								if r := recover(); r != nil {
									ret = result.Err[SdkResultStruct](
										errors.Join(
											fmt.Errorf(contracts.SDK_ERROR),
											fmt.Errorf("SYSTEM_CALL_PANIC"),
											fmt.Errorf("%v", r),
										),
									)
								}
							}()

							out := fn(ctx, valArg.Arg0)
							if out.IsErr() {
								return out
							}
							unwrapped := out.Unwrap()
							payload, err := json.Marshal(map[string]string{"result": unwrapped.Result})
							if err != nil {
								return result.Err[SdkResultStruct](
									errors.Join(
										fmt.Errorf(contracts.SDK_ERROR),
										fmt.Errorf("SERIALIZATION_ERROR"),
										err,
									),
								)
							}
							unwrapped.Result = string(payload)
							return result.Ok(unwrapped)
						}()
					}
				}
			}
			return result.Err[SdkResultStruct](errors.Join(fmt.Errorf(contracts.SDK_ERROR), fmt.Errorf("INVALID_CALL")))
		},
		"get_env_key": func(ctx context.Context, a any) SdkResult {
			eCtx := ctx.Value(wasm_context.WasmExecCtxKey).(wasm_context.ExecContextValue)
			envArg, ok := a.(string)
			if !ok {
				return ErrInvalidArgument
			}
			session := eCtx.IOSession()
			return result.Map(
				eCtx.EnvVar(envArg),
				func(s string) SdkResultStruct { return SdkResultStruct{Result: s, Gas: session.End()} },
			)
		},
		"get_env": func(ctx context.Context, a any) SdkResult {
			eCtx := ctx.Value(wasm_context.WasmExecCtxKey).(wasm_context.ExecContextValue)
			session := eCtx.IOSession()
			return result.Map(
				eCtx.GetEnv(),
				func(s string) SdkResultStruct { return SdkResultStruct{Result: s, Gas: session.End()} },
			)
		},
		"verify_address": func(ctx context.Context, a any) SdkResult {
			addr, ok := a.(string)
			if !ok {
				return ErrInvalidArgument
			}
			return result.Ok(
				SdkResultStruct{Result: dids.VerifyAddress(addr, onMainnet), Gas: params.CYCLE_GAS_PER_RC / 4},
			)
		},
		// pendulum_apply_swap_fees: pool contracts call this from inside their
		// swap handler with just (asset_in, asset_out, x, x_reserve, y_reserve,
		// exacerbates). The SDK derives gross output, both base fees, the
		// stabilizer surplus, the network cut, and the pendulum split — all on
		// the output side, matching the existing pre-pendulum contract math.
		// Returns JSON-encoded PendulumSwapFeeOutput.
		// Whitelist + math + node-side accrual all happen inside the applier
		// behind eCtx — the SDK function itself is a thin marshalling shim.
		"pendulum_apply_swap_fees": func(ctx context.Context, a any) SdkResult {
			eCtx := ctx.Value(wasm_context.WasmExecCtxKey).(wasm_context.ExecContextValue)
			payload, ok := a.(string)
			if !ok {
				return ErrInvalidArgument
			}
			var in pendulumSwapFeeInput
			if err := json.Unmarshal([]byte(payload), &in); err != nil {
				return result.Err[SdkResultStruct](
					errors.Join(fmt.Errorf(contracts.SDK_ERROR), fmt.Errorf("INVALID_ARGUMENT_JSON"), err),
				)
			}
			x, ok := parseInt64(in.X)
			if !ok {
				return ErrInvalidArgument
			}
			xRes, ok := parseInt64(in.XReserve)
			if !ok {
				return ErrInvalidArgument
			}
			yRes, ok := parseInt64(in.YReserve)
			if !ok {
				return ErrInvalidArgument
			}
			session := eCtx.IOSession()
			res := eCtx.PendulumApplySwapFees(wasm_context.PendulumSwapFeeArgs{
				AssetIn:  in.AssetIn,
				AssetOut: in.AssetOut,
				X:        x,
				XReserve: xRes,
				YReserve: yRes,
			})
			return result.Map(res, func(r wasm_context.PendulumSwapFeeResult) SdkResultStruct {
				out := pendulumSwapFeeOutput{
					UserOutput:            strconv.FormatInt(r.UserOutput, 10),
					NewXReserve:           strconv.FormatInt(r.NewXReserve, 10),
					NewYReserve:           strconv.FormatInt(r.NewYReserve, 10),
					NodeBucketCreditedHBD: strconv.FormatInt(r.NodeBucketCreditedHBD, 10),
					MultiplierBps:         strconv.FormatInt(r.MultiplierBps, 10),
					SAfterBps:             strconv.FormatInt(r.SAfterBps, 10),
					NetworkCreditOutput:   strconv.FormatInt(r.NetworkCreditOutput, 10),
					LpShareOutput:         strconv.FormatInt(r.LpShareOutput, 10),
					NodeShareOutput:       strconv.FormatInt(r.NodeShareOutput, 10),
				}
				js, _ := json.Marshal(out)
				return SdkResultStruct{Result: string(js), Gas: session.End()}
			})
		},
	},

	// -------------------------------------------------------------------------
	// hive — token operations
	// -------------------------------------------------------------------------
	"hive": {
		"get_balance": func(ctx context.Context, arg1 any, arg2 any) SdkResult {
			eCtx := ctx.Value(wasm_context.WasmExecCtxKey).(wasm_context.ExecContextValue)
			account, ok := arg1.(string)
			if !ok {
				return ErrInvalidArgument
			}
			asset, ok := arg2.(string)
			if !ok {
				return ErrInvalidArgument
			}
			return result.Ok(SdkResultStruct{
				Result: fmt.Sprint(eCtx.GetBalance(account, asset)),
				Gas:    params.CYCLE_GAS_PER_RC / 2,
			})
		},
		"draw": func(ctx context.Context, arg1 any, arg2 any) SdkResult {
			eCtx := ctx.Value(wasm_context.WasmExecCtxKey).(wasm_context.ExecContextValue)
			amountString, ok := arg1.(string)
			if !ok {
				return ErrInvalidArgument
			}
			amount, err := strconv.ParseInt(amountString, 10, 64)
			if err != nil {
				return result.Err[SdkResultStruct](errors.Join(fmt.Errorf(contracts.SDK_ERROR), err))
			}
			if amount <= 0 {
				return result.Err[SdkResultStruct](
					errors.Join(fmt.Errorf(contracts.SDK_ERROR), fmt.Errorf("amount must be positive")),
				)
			}
			asset, ok := arg2.(string)
			if !ok {
				return ErrInvalidArgument
			}
			return result.Map(
				eCtx.PullBalance("", amount, asset),
				func(struct{}) SdkResultStruct { return SdkResultStruct{Gas: params.CYCLE_GAS_PER_RC} },
			)
		},
		"transfer": func(ctx context.Context, arg1 any, arg2 any, arg3 any) SdkResult {
			eCtx := ctx.Value(wasm_context.WasmExecCtxKey).(wasm_context.ExecContextValue)
			to, ok := arg1.(string)
			if !ok {
				return ErrInvalidArgument
			}
			amountString, ok := arg2.(string)
			if !ok {
				return ErrInvalidArgument
			}
			amount, err := strconv.ParseInt(amountString, 10, 64)
			if err != nil {
				return result.Err[SdkResultStruct](errors.Join(fmt.Errorf(contracts.SDK_ERROR), err))
			}
			if amount <= 0 {
				return result.Err[SdkResultStruct](
					errors.Join(fmt.Errorf(contracts.SDK_ERROR), fmt.Errorf("amount must be positive")),
				)
			}
			asset, ok := arg3.(string)
			if !ok {
				return ErrInvalidArgument
			}
			return result.Map(
				eCtx.SendBalance(to, amount, asset),
				func(struct{}) SdkResultStruct { return SdkResultStruct{Gas: params.CYCLE_GAS_PER_RC} },
			)
		},
		"withdraw": func(ctx context.Context, arg1 any, arg2 any, arg3 any) SdkResult {
			eCtx := ctx.Value(wasm_context.WasmExecCtxKey).(wasm_context.ExecContextValue)
			to, ok := arg1.(string)
			if !ok {
				return ErrInvalidArgument
			}
			amountString, ok := arg2.(string)
			if !ok {
				return ErrInvalidArgument
			}
			amount, err := strconv.ParseInt(amountString, 10, 64)
			if err != nil {
				return result.Err[SdkResultStruct](errors.Join(fmt.Errorf(contracts.SDK_ERROR), err))
			}
			if amount <= 0 {
				return result.Err[SdkResultStruct](
					errors.Join(fmt.Errorf(contracts.SDK_ERROR), fmt.Errorf("amount must be positive")),
				)
			}
			asset, ok := arg3.(string)
			if !ok {
				return ErrInvalidArgument
			}
			return result.Map(
				eCtx.WithdrawBalance(to, amount, asset),
				func(struct{}) SdkResultStruct { return SdkResultStruct{Gas: 5 * params.CYCLE_GAS_PER_RC} },
			)
		},
	},

	// -------------------------------------------------------------------------
	// contracts — inter-contract calls
	// -------------------------------------------------------------------------
	"contracts": {
		"read": func(ctx context.Context, arg1 any, arg2 any) SdkResult {
			eCtx := ctx.Value(wasm_context.WasmExecCtxKey).(wasm_context.ExecContextValue)
			contractId, ok := arg1.(string)
			if !ok {
				return ErrInvalidArgument
			}
			key, ok := arg2.(string)
			if !ok {
				return ErrInvalidArgument
			}
			session := eCtx.IOSession()
			return result.Map(
				eCtx.ContractStateGet(contractId, key),
				func(r string) SdkResultStruct { return SdkResultStruct{Result: r, Gas: session.End()} },
			)
		},
		"call": func(ctx context.Context, arg1 any, arg2 any, arg3 any, arg4 any) SdkResult {
			eCtx := ctx.Value(wasm_context.WasmExecCtxKey).(wasm_context.ExecContextValue)
			contractId, ok := arg1.(string)
			if !ok {
				return ErrInvalidArgument
			}
			method, ok := arg2.(string)
			if !ok {
				return ErrInvalidArgument
			}
			payload, ok := arg3.(string)
			if !ok {
				return ErrInvalidArgument
			}
			options, ok := arg4.(string)
			if !ok {
				return ErrInvalidArgument
			}
			return eCtx.ContractCall(contractId, method, payload, options)
		},
	},

	// -------------------------------------------------------------------------
	// tss — threshold signature operations (v1, backward compatible)
	// -------------------------------------------------------------------------
	"tss": {
		// create_key — legacy 2-arg form; defaults epochs to MaxKeyEpochs.
		// Kept for contracts compiled against the original SDK.
		"create_key": func(ctx context.Context, arg1 any, arg2 any) SdkResult {
			keyName, ok := arg1.(string)
			if !ok {
				return ErrInvalidArgument
			}
			matched, _ := regexp.Match("^[a-zA-Z0-9]+$", []byte(keyName))
			if !matched {
				return ErrInvalidArgument
			}
			keyType, ok := arg2.(string)
			if !ok {
				return ErrInvalidArgument
			}
			if keyType != "eddsa" && keyType != "ecdsa" {
				return ErrInvalidArgument
			}
			eCtx := ctx.Value(wasm_context.WasmExecCtxKey).(wasm_context.ExecContextValue)
			res := eCtx.TssCreateKey(keyName, keyType, tss_db.MaxKeyEpochs)
			if res.IsOk() {
				return result.Ok(SdkResultStruct{Result: res.Unwrap(), Gas: 1_000_000})
			}
			return result.Err[SdkResultStruct](res.UnwrapErr())
		},
		"sign_key": func(ctx context.Context, arg1 any, arg2 any) SdkResult {
			keyId, ok := arg1.(string)
			if !ok {
				return ErrInvalidArgument
			}
			signPayload, ok := arg2.(string)
			if !ok {
				return ErrInvalidArgument
			}
			eCtx := ctx.Value(wasm_context.WasmExecCtxKey).(wasm_context.ExecContextValue)
			res := eCtx.TssKeySign(keyId, signPayload)
			return result.Ok(SdkResultStruct{Result: res.Unwrap(), Gas: 100_000})
		},
		"get_key": func(ctx context.Context, arg1 any) SdkResult {
			keyId, ok := arg1.(string)
			if !ok {
				return ErrInvalidArgument
			}
			eCtx := ctx.Value(wasm_context.WasmExecCtxKey).(wasm_context.ExecContextValue)
			res := eCtx.TssGetKey(keyId)
			return result.Ok(SdkResultStruct{Result: res.Unwrap(), Gas: 10_000})
		},
	},

	// -------------------------------------------------------------------------
	// tss_v2 — TSS operations with explicit epoch control
	// -------------------------------------------------------------------------
	"tss_v2": {
		// create_key — 3-arg form with explicit epochs.
		"create_key": func(ctx context.Context, arg1 any, arg2 any, arg3 any) SdkResult {
			keyName, ok := arg1.(string)
			if !ok {
				return ErrInvalidArgument
			}
			matched, _ := regexp.Match("^[a-zA-Z0-9]+$", []byte(keyName))
			if !matched {
				return ErrInvalidArgument
			}
			keyType, ok := arg2.(string)
			if !ok {
				return ErrInvalidArgument
			}
			if keyType != "eddsa" && keyType != "ecdsa" {
				return ErrInvalidArgument
			}
			epochsStr, ok := arg3.(string)
			if !ok {
				return ErrInvalidArgument
			}
			epochs, parseErr := strconv.ParseUint(epochsStr, 10, 64)
			if parseErr != nil || epochs == 0 || epochs > tss_db.MaxKeyEpochs {
				return ErrInvalidArgument
			}
			eCtx := ctx.Value(wasm_context.WasmExecCtxKey).(wasm_context.ExecContextValue)
			res := eCtx.TssCreateKey(keyName, keyType, epochs)
			if res.IsOk() {
				return result.Ok(SdkResultStruct{Result: res.Unwrap(), Gas: 1_000_000})
			}
			return result.Err[SdkResultStruct](res.UnwrapErr())
		},
		"renew_key": func(ctx context.Context, arg1 any, arg2 any) SdkResult {
			keyName, ok := arg1.(string)
			if !ok {
				return ErrInvalidArgument
			}
			matched, _ := regexp.Match("^[a-zA-Z0-9]+$", []byte(keyName))
			if !matched {
				return ErrInvalidArgument
			}
			epochsStr, ok := arg2.(string)
			if !ok {
				return ErrInvalidArgument
			}
			epochs, parseErr := strconv.ParseUint(epochsStr, 10, 64)
			if parseErr != nil || epochs == 0 || epochs > tss_db.MaxKeyEpochs {
				return ErrInvalidArgument
			}
			eCtx := ctx.Value(wasm_context.WasmExecCtxKey).(wasm_context.ExecContextValue)
			res := eCtx.TssRenewKey(keyName, epochs)
			if res.IsOk() {
				return result.Ok(SdkResultStruct{Result: res.Unwrap(), Gas: 100_000})
			}
			return result.Err[SdkResultStruct](res.UnwrapErr())
		},
	},

	// -------------------------------------------------------------------------
	// crypto — pure cryptographic operations run on the host, not in WASM
	// -------------------------------------------------------------------------
	"crypto": {
		"keccak256": func(ctx context.Context, arg1 any) SdkResult {
			hexData, ok := arg1.(string)
			if !ok {
				return ErrInvalidArgument
			}
			data, err := hexDecode(hexData)
			if err != nil {
				return result.Err[SdkResultStruct](
					errors.Join(fmt.Errorf(contracts.SDK_ERROR), fmt.Errorf("invalid hex")),
				)
			}
			hash := ethCrypto.Keccak256(data)
			return result.Ok(SdkResultStruct{
				Result: hexEncode(hash),
				Gas:    params.CYCLE_GAS_PER_RC/4 + uint(len(data))*keccak256GasPerByte,
			})
		},
		"ecrecover": func(ctx context.Context, arg1 any, arg2 any) SdkResult {
			hashHex, ok := arg1.(string)
			if !ok {
				return ErrInvalidArgument
			}
			sigHex, ok := arg2.(string)
			if !ok {
				return ErrInvalidArgument
			}
			hash, err := hexDecode(hashHex)
			if err != nil || len(hash) != 32 {
				return result.Err[SdkResultStruct](
					errors.Join(fmt.Errorf(contracts.SDK_ERROR), fmt.Errorf("hash must be 32 bytes hex")),
				)
			}
			sig, err := hexDecode(sigHex)
			if err != nil || len(sig) != 65 {
				return result.Err[SdkResultStruct](
					errors.Join(fmt.Errorf(contracts.SDK_ERROR), fmt.Errorf("sig must be 65 bytes hex (r+s+v)")),
				)
			}
			pubkey, err := ethCrypto.Ecrecover(hash, sig)
			if err != nil {
				return result.Err[SdkResultStruct](
					errors.Join(fmt.Errorf(contracts.SDK_ERROR), fmt.Errorf("ecrecover failed: %w", err)),
				)
			}
			addr := ethCrypto.Keccak256(pubkey[1:])[12:]
			return result.Ok(SdkResultStruct{Result: hexEncode(addr), Gas: params.CYCLE_GAS_PER_RC})
		},
		"rlp_decode": func(ctx context.Context, arg1 any) SdkResult {
			hexData, ok := arg1.(string)
			if !ok {
				return ErrInvalidArgument
			}
			data, err := hexDecode(hexData)
			if err != nil {
				return result.Err[SdkResultStruct](
					errors.Join(fmt.Errorf(contracts.SDK_ERROR), fmt.Errorf("invalid hex")),
				)
			}
			decoded, err := rlpDecodeToJSON(data)
			if err != nil {
				return result.Err[SdkResultStruct](
					errors.Join(fmt.Errorf(contracts.SDK_ERROR), fmt.Errorf("rlp decode failed: %w", err)),
				)
			}
			return result.Ok(SdkResultStruct{
				Result: decoded,
				Gas:    params.CYCLE_GAS_PER_RC/4 + uint(len(data))*rlpDecodeGasPerByte,
			})
		},
		"sp1_verify_groth16": func(ctx context.Context, arg1 any, arg2 any, arg3 any, arg4 any, arg5 any) SdkResult {
			proofHex, ok := arg1.(string)
			if !ok {
				return ErrInvalidArgument
			}
			publicInputsHex, ok := arg2.(string)
			if !ok {
				return ErrInvalidArgument
			}
			sp1VkeyHashHex, ok := arg3.(string)
			if !ok {
				return ErrInvalidArgument
			}
			groth16VkHex, ok := arg4.(string)
			if !ok {
				return ErrInvalidArgument
			}
			vkRootHex, ok := arg5.(string)
			if !ok {
				return ErrInvalidArgument
			}
			proof, err := hexDecode(proofHex)
			if err != nil {
				return result.Err[SdkResultStruct](
					errors.Join(fmt.Errorf(contracts.SDK_ERROR), fmt.Errorf("invalid proof hex")),
				)
			}
			publicInputs, err := hexDecode(publicInputsHex)
			if err != nil {
				return result.Err[SdkResultStruct](
					errors.Join(fmt.Errorf(contracts.SDK_ERROR), fmt.Errorf("invalid public inputs hex")),
				)
			}
			vkeyHash, err := hexDecode(sp1VkeyHashHex)
			if err != nil {
				return result.Err[SdkResultStruct](
					errors.Join(fmt.Errorf(contracts.SDK_ERROR), fmt.Errorf("invalid vkey hash hex")),
				)
			}
			groth16Vk, err := hexDecode(groth16VkHex)
			if err != nil {
				return result.Err[SdkResultStruct](
					errors.Join(fmt.Errorf(contracts.SDK_ERROR), fmt.Errorf("invalid groth16 vk hex")),
				)
			}
			vkRoot, err := hexDecode(vkRootHex)
			if err != nil {
				return result.Err[SdkResultStruct](
					errors.Join(fmt.Errorf(contracts.SDK_ERROR), fmt.Errorf("invalid vk root hex")),
				)
			}
			if err := Sp1VerifyGroth16(proof, publicInputs, vkeyHash, groth16Vk, vkRoot); err != nil {
				return result.Ok(SdkResultStruct{Result: "false", Gas: params.CYCLE_GAS_PER_RC * 10})
			}
			return result.Ok(SdkResultStruct{Result: "true", Gas: params.CYCLE_GAS_PER_RC * 10})
		},
		// bls_verify performs single-pubkey BLS12-381 signature verification
		// over the message digest. All inputs are hex-encoded:
		//   pubkey: 48-byte compressed G1 point (96 hex chars)
		//   msg:    arbitrary length message (varies)
		//   sig:    96-byte compressed G2 point (192 hex chars)
		// Returns "true" or "false". Deterministic — same inputs always
		// produce the same output, safe for consensus use.
		//
		// Backed by the existing protolambda/bls12-381-util library used by
		// Magi consensus, so the verification path matches what block voting
		// and BlsCircuit.Verify already rely on.
		//
		// Gas: ~5x keccak256, reflecting the ~1-2ms verify cost for a
		// single point pair. Use bls_verify_aggregate when you need to
		// verify a quorum-aggregated signature over the same message.
		"bls_verify": func(ctx context.Context, arg1 any, arg2 any, arg3 any) SdkResult {
			pubkeyHex, ok := arg1.(string)
			if !ok {
				return ErrInvalidArgument
			}
			msgHex, ok := arg2.(string)
			if !ok {
				return ErrInvalidArgument
			}
			sigHex, ok := arg3.(string)
			if !ok {
				return ErrInvalidArgument
			}
			pubkey, err := blsParsePubkey(pubkeyHex)
			if err != nil {
				return result.Err[SdkResultStruct](
					errors.Join(fmt.Errorf(contracts.SDK_ERROR), err),
				)
			}
			msg, err := hexDecode(msgHex)
			if err != nil {
				return result.Err[SdkResultStruct](
					errors.Join(fmt.Errorf(contracts.SDK_ERROR), fmt.Errorf("invalid msg hex")),
				)
			}
			sig, err := blsParseSignature(sigHex)
			if err != nil {
				return result.Err[SdkResultStruct](
					errors.Join(fmt.Errorf(contracts.SDK_ERROR), err),
				)
			}
			ok = bls.Verify(pubkey, msg, sig)
			outResult := "false"
			if ok {
				outResult = "true"
			}
			return result.Ok(SdkResultStruct{
				Result: outResult,
				Gas:    params.CYCLE_GAS_PER_RC * 5,
			})
		},
		// bls_verify_aggregate verifies an aggregated BLS signature against
		// a list of pubkeys all having signed the SAME message. This is the
		// standard "quorum signed a notarization" pattern: each validator
		// signs the canonical message with its own key, then the submitter
		// aggregates the signatures (via bls.Aggregate off-chain) and the
		// contract aggregates the pubkeys here for a single pairing check.
		//
		// pubkeys_concat: concatenated hex of 48-byte compressed pubkeys
		//                 (length must be a multiple of 96 hex chars, at
		//                 least 1 pubkey, max 256 to bound CPU cost)
		// msg:            arbitrary length message (hex)
		// agg_sig:        96-byte compressed aggregate signature (hex)
		//
		// Returns "true" or "false". Deterministic.
		//
		// Gas: base 10x keccak + 0.5x keccak per pubkey aggregated, reflecting
		// per-pubkey hash + EC add cost on top of the verify pairing.
		//
		// Designed for the Dash InstantSend login attestation flow where N
		// Magi validators each sign the same canonical IS-lock message with
		// their consensus BLS key. The dash-mapping-contract reads the
		// validator set from elections state, supplies their pubkeys here,
		// and verifies the bundle.
		"bls_verify_aggregate": func(ctx context.Context, arg1 any, arg2 any, arg3 any) SdkResult {
			pubkeysHex, ok := arg1.(string)
			if !ok {
				return ErrInvalidArgument
			}
			msgHex, ok := arg2.(string)
			if !ok {
				return ErrInvalidArgument
			}
			aggSigHex, ok := arg3.(string)
			if !ok {
				return ErrInvalidArgument
			}
			pubkeys, err := blsParsePubkeysConcat(pubkeysHex)
			if err != nil {
				return result.Err[SdkResultStruct](
					errors.Join(fmt.Errorf(contracts.SDK_ERROR), err),
				)
			}
			msg, err := hexDecode(msgHex)
			if err != nil {
				return result.Err[SdkResultStruct](
					errors.Join(fmt.Errorf(contracts.SDK_ERROR), fmt.Errorf("invalid msg hex")),
				)
			}
			aggSig, err := blsParseSignature(aggSigHex)
			if err != nil {
				return result.Err[SdkResultStruct](
					errors.Join(fmt.Errorf(contracts.SDK_ERROR), err),
				)
			}
			aggPubkey, err := bls.AggregatePubkeys(pubkeys)
			if err != nil {
				return result.Err[SdkResultStruct](
					errors.Join(fmt.Errorf(contracts.SDK_ERROR), fmt.Errorf("aggregate pubkeys: %w", err)),
				)
			}
			verified := bls.Verify(aggPubkey, msg, aggSig)
			outResult := "false"
			if verified {
				outResult = "true"
			}
			// Per-pubkey hash + curve add cost is roughly half a keccak each;
			// the pairing dominates total cost regardless of N up to small
			// quorum sizes (~17 for testnet, similar for mainnet).
			perPubkeyGas := uint(params.CYCLE_GAS_PER_RC / 2)
			return result.Ok(SdkResultStruct{
				Result: outResult,
				Gas:    params.CYCLE_GAS_PER_RC*10 + perPubkeyGas*uint(len(pubkeys)),
			})
		},
	},
}

// blsMaxPubkeysPerVerify caps the aggregate-verify size to bound CPU cost.
// Sized comfortably above current Magi validator set (17 on testnet); raise
// if validator counts grow.
const blsMaxPubkeysPerVerify = 256

// blsParsePubkey decodes a single 48-byte compressed G1 pubkey from hex.
func blsParsePubkey(hexStr string) (*bls.Pubkey, error) {
	raw, err := hexDecode(hexStr)
	if err != nil {
		return nil, fmt.Errorf("invalid pubkey hex")
	}
	if len(raw) != 48 {
		return nil, fmt.Errorf("pubkey must be 48 bytes hex, got %d", len(raw))
	}
	var pk bls.Pubkey
	if err := pk.Deserialize((*[48]byte)(raw)); err != nil {
		return nil, fmt.Errorf("pubkey deserialize: %w", err)
	}
	return &pk, nil
}

// blsParsePubkeysConcat decodes N concatenated 48-byte compressed G1
// pubkeys from a single hex string. Length must be a multiple of 96 hex
// chars, at least 1 pubkey, at most blsMaxPubkeysPerVerify.
func blsParsePubkeysConcat(hexStr string) ([]*bls.Pubkey, error) {
	raw, err := hexDecode(hexStr)
	if err != nil {
		return nil, fmt.Errorf("invalid pubkeys hex")
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("pubkeys empty: need at least 1")
	}
	if len(raw)%48 != 0 {
		return nil, fmt.Errorf("pubkeys length must be multiple of 48 bytes, got %d", len(raw))
	}
	n := len(raw) / 48
	if n > blsMaxPubkeysPerVerify {
		return nil, fmt.Errorf("pubkeys count %d exceeds max %d", n, blsMaxPubkeysPerVerify)
	}
	out := make([]*bls.Pubkey, n)
	for i := 0; i < n; i++ {
		var pk bls.Pubkey
		start := i * 48
		chunk := raw[start : start+48]
		if err := pk.Deserialize((*[48]byte)(chunk)); err != nil {
			return nil, fmt.Errorf("pubkey %d deserialize: %w", i, err)
		}
		out[i] = &pk
	}
	return out, nil
}

// blsParseSignature decodes a single 96-byte compressed G2 signature from hex.
func blsParseSignature(hexStr string) (*bls.Signature, error) {
	raw, err := hexDecode(hexStr)
	if err != nil {
		return nil, fmt.Errorf("invalid sig hex")
	}
	if len(raw) != 96 {
		return nil, fmt.Errorf("sig must be 96 bytes hex, got %d", len(raw))
	}
	var sig bls.Signature
	if err := sig.Deserialize((*[96]byte)(raw)); err != nil {
		return nil, fmt.Errorf("sig deserialize: %w", err)
	}
	return &sig, nil
}

func hexDecode(s string) ([]byte, error) {
	s = strings.TrimPrefix(s, "0x")
	return hex.DecodeString(s)
}

func hexEncode(b []byte) string {
	return hex.EncodeToString(b)
}

// Per-byte gas for host-side crypto. Keccak256 and RLP decode are both O(n)
// in input size; charging constant gas lets a contract DoS the node with
// large inputs. Values picked so that a 10 KB input costs roughly one RC.
const (
	keccak256GasPerByte = params.CYCLE_GAS_PER_RC / 256
	rlpDecodeGasPerByte = params.CYCLE_GAS_PER_RC / 128
)

// rlpMaxDecodeDepth bounds recursion to prevent stack exhaustion. Ethereum
// transactions reach depth 3 (tx → accessList → [addr, [storageKeys]]); 16
// is comfortably above any legitimate payload.
const rlpMaxDecodeDepth = 16

// rlpDecodeToJSON decodes RLP into a JSON document where byte strings become
// hex-encoded JSON strings and lists become JSON arrays. Output is
// consensus-critical: every node must produce byte-identical JSON.
func rlpDecodeToJSON(data []byte) (string, error) {
	stream := rlp.NewStream(bytes.NewReader(data), uint64(len(data)))
	val, err := decodeRlpValue(stream, 0)
	if err != nil {
		return "", err
	}
	// Reject trailing bytes — only a single top-level RLP item is valid.
	if _, _, err := stream.Kind(); err != io.EOF {
		if err == nil {
			return "", fmt.Errorf("trailing data after RLP item")
		}
		return "", err
	}
	out, err := json.Marshal(val)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func decodeRlpValue(s *rlp.Stream, depth int) (interface{}, error) {
	if depth > rlpMaxDecodeDepth {
		return nil, fmt.Errorf("max nesting depth %d exceeded", rlpMaxDecodeDepth)
	}
	kind, _, err := s.Kind()
	if err != nil {
		return nil, err
	}
	switch kind {
	case rlp.Byte, rlp.String:
		b, err := s.Bytes()
		if err != nil {
			return nil, err
		}
		return hex.EncodeToString(b), nil
	case rlp.List:
		if _, err := s.List(); err != nil {
			return nil, err
		}
		arr := []interface{}{}
		for {
			item, err := decodeRlpValue(s, depth+1)
			if err == rlp.EOL {
				break
			}
			if err != nil {
				return nil, err
			}
			arr = append(arr, item)
		}
		if err := s.ListEnd(); err != nil {
			return nil, err
		}
		return arr, nil
	}
	return nil, fmt.Errorf("unexpected RLP kind %v", kind)
}

// pendulumSwapFeeInput is the JSON shape contracts pass to
// system.pendulum_apply_swap_fees. All amounts are decimal strings to
// keep precision (wasm guests typically pass big-int-like values). The
// SDK derives gross output, both base fees, the stabilizer push
// direction, and the surplus internally — the contract only supplies
// the swap inputs.
type pendulumSwapFeeInput struct {
	AssetIn  string `json:"asset_in"`
	AssetOut string `json:"asset_out"`
	X        string `json:"x"`
	XReserve string `json:"x_reserve"`
	YReserve string `json:"y_reserve"`
}

// pendulumSwapFeeOutput is the JSON shape returned to contracts.
// network_credit_output is the 25% network cut on (totalCLP + totalProtocol),
// in the output asset of the swap (both fee components live on the output
// side under the unified model).
type pendulumSwapFeeOutput struct {
	UserOutput            string `json:"user_output"`
	NewXReserve           string `json:"new_x_reserve"`
	NewYReserve           string `json:"new_y_reserve"`
	NodeBucketCreditedHBD string `json:"node_bucket_credited_hbd"`
	MultiplierBps         string `json:"multiplier_bps"`
	SAfterBps             string `json:"s_after_bps"`
	NetworkCreditOutput   string `json:"network_credit_output"`
	LpShareOutput         string `json:"lp_share_output"`
	NodeShareOutput       string `json:"node_share_output"`
}

func parseInt64(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
