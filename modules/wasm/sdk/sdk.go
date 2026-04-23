package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"vsc-node/lib/dids"
	"vsc-node/modules/common/params"
	"vsc-node/modules/db/vsc/contracts"
	tss_db "vsc-node/modules/db/vsc/tss"
	wasm_context "vsc-node/modules/wasm/context"
	wasm_types "vsc-node/modules/wasm/types"

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
										errors.Join(fmt.Errorf(contracts.SDK_ERROR), fmt.Errorf("SYSTEM_CALL_PANIC"), fmt.Errorf("%v", r)),
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
									errors.Join(fmt.Errorf(contracts.SDK_ERROR), fmt.Errorf("SERIALIZATION_ERROR"), err),
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
}
