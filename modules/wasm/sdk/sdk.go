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

	ethCrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
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
				return result.Err[SdkResultStruct](errors.Join(fmt.Errorf(contracts.SDK_ERROR), fmt.Errorf("invalid hex")))
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
				return result.Err[SdkResultStruct](errors.Join(fmt.Errorf(contracts.SDK_ERROR), fmt.Errorf("hash must be 32 bytes hex")))
			}
			sig, err := hexDecode(sigHex)
			if err != nil || len(sig) != 65 {
				return result.Err[SdkResultStruct](errors.Join(fmt.Errorf(contracts.SDK_ERROR), fmt.Errorf("sig must be 65 bytes hex (r+s+v)")))
			}
			pubkey, err := ethCrypto.Ecrecover(hash, sig)
			if err != nil {
				return result.Err[SdkResultStruct](errors.Join(fmt.Errorf(contracts.SDK_ERROR), fmt.Errorf("ecrecover failed: %w", err)))
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
				return result.Err[SdkResultStruct](errors.Join(fmt.Errorf(contracts.SDK_ERROR), fmt.Errorf("invalid hex")))
			}
			decoded, err := rlpDecodeToJSON(data)
			if err != nil {
				return result.Err[SdkResultStruct](errors.Join(fmt.Errorf(contracts.SDK_ERROR), fmt.Errorf("rlp decode failed: %w", err)))
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
				return result.Err[SdkResultStruct](errors.Join(fmt.Errorf(contracts.SDK_ERROR), fmt.Errorf("invalid proof hex")))
			}
			publicInputs, err := hexDecode(publicInputsHex)
			if err != nil {
				return result.Err[SdkResultStruct](errors.Join(fmt.Errorf(contracts.SDK_ERROR), fmt.Errorf("invalid public inputs hex")))
			}
			vkeyHash, err := hexDecode(sp1VkeyHashHex)
			if err != nil {
				return result.Err[SdkResultStruct](errors.Join(fmt.Errorf(contracts.SDK_ERROR), fmt.Errorf("invalid vkey hash hex")))
			}
			groth16Vk, err := hexDecode(groth16VkHex)
			if err != nil {
				return result.Err[SdkResultStruct](errors.Join(fmt.Errorf(contracts.SDK_ERROR), fmt.Errorf("invalid groth16 vk hex")))
			}
			vkRoot, err := hexDecode(vkRootHex)
			if err != nil {
				return result.Err[SdkResultStruct](errors.Join(fmt.Errorf(contracts.SDK_ERROR), fmt.Errorf("invalid vk root hex")))
			}
			if err := Sp1VerifyGroth16(proof, publicInputs, vkeyHash, groth16Vk, vkRoot); err != nil {
				return result.Ok(SdkResultStruct{Result: "false", Gas: params.CYCLE_GAS_PER_RC * 10})
			}
			return result.Ok(SdkResultStruct{Result: "true", Gas: params.CYCLE_GAS_PER_RC * 10})
		},
	},
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
