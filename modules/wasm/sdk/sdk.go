package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"vsc-node/modules/common/params"
	"vsc-node/modules/db/vsc/contracts"
	wasm_context "vsc-node/modules/wasm/context"
	wasm_types "vsc-node/modules/wasm/types"

	"github.com/JustinKnueppel/go-result"
)

type SdkResultStruct = wasm_types.WasmResultStruct
type SdkResult = result.Result[SdkResultStruct]

var (
	ErrInvalidArgument = result.Err[SdkResultStruct](errors.Join(fmt.Errorf(contracts.SDK_ERROR), fmt.Errorf("invalid argument")))
	ErrUnimplemented   = result.Err[SdkResultStruct](errors.Join(fmt.Errorf(contracts.SDK_ERROR), fmt.Errorf("unimplemented")))
)

var sdkModuleRef *map[string]sdkFunc

func init() {
	sdkModuleRef = &SdkModule
}

type sdkFunc any

// func(context.Context, args ...any) SdkResult

var SdkModule = map[string]sdkFunc{
	"console.log": func(ctx context.Context, a any) SdkResult {
		eCtx := ctx.Value(wasm_context.WasmExecCtxKey).(wasm_context.ExecContextValue)
		s, ok := a.(string)
		if !ok {
			return ErrInvalidArgument
		}

		// fmt.Println("console.log(s):", s)
		session := eCtx.IOSession()
		eCtx.Log(s)
		gas := session.End()
		return result.Ok(SdkResultStruct{
			Gas: gas,
		})
	},
	"db.set_object": func(ctx context.Context, arg1 any, arg2 any) SdkResult {
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
			func(struct{}) SdkResultStruct {
				return SdkResultStruct{
					Gas: session.End(),
				}
			},
		)
	},
	"db.get_object": func(ctx context.Context, a any) SdkResult {
		eCtx := ctx.Value(wasm_context.WasmExecCtxKey).(wasm_context.ExecContextValue)
		key, ok := a.(string)
		if !ok {
			return ErrInvalidArgument
		}

		session := eCtx.IOSession()
		return result.Map(
			eCtx.GetState(key),
			func(s string) SdkResultStruct {
				return SdkResultStruct{
					Result: s,
					Gas:    session.End(),
				}
			},
		)
	},
	"db.rm_object": func(ctx context.Context, a any) SdkResult {
		eCtx := ctx.Value(wasm_context.WasmExecCtxKey).(wasm_context.ExecContextValue)
		key, ok := a.(string)
		if !ok {
			return ErrInvalidArgument
		}

		session := eCtx.IOSession()
		return result.Map(
			eCtx.DeleteState(key),
			func(struct{}) SdkResultStruct {
				return SdkResultStruct{
					Gas: session.End(),
				}
			},
		)
	},
	"system.call": func(ctx context.Context, arg1 any, arg2 any) SdkResult {
		/*eCtx :*/ _ = ctx.Value(wasm_context.WasmExecCtxKey).(wasm_context.ExecContextValue)
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
		err := json.Unmarshal([]byte(rawValArg), &valArg)

		f, ok := (*sdkModuleRef)[callArg]
		if ok {
			return result.And(
				result.Err[any](err),
				result.Map(
					f.(func(context.Context, any) SdkResult)(ctx, valArg.Arg0),
					func(res SdkResultStruct) SdkResultStruct {
						res.Result = fmt.Sprintf(`{"result":"%s"}`, res.Result)
						return res
					},
				),
			)
		} else {
			return result.Err[SdkResultStruct](errors.Join(fmt.Errorf(contracts.SDK_ERROR), fmt.Errorf("INVALID_CALL")))
		}
	},
	"system.get_env_key": func(ctx context.Context, a any) SdkResult {
		eCtx := ctx.Value(wasm_context.WasmExecCtxKey).(wasm_context.ExecContextValue)
		envArg, ok := a.(string)
		if !ok {
			return ErrInvalidArgument
		}
		session := eCtx.IOSession()
		return result.Map(
			eCtx.EnvVar(envArg),
			// data,
			func(s string) SdkResultStruct {
				gas := session.End()

				// fmt.Println("system.getEnv", envArg, s, gas)
				return SdkResultStruct{
					Result: s,
					Gas:    gas,
				}
			},
		)
	},
	"system.get_env": func(ctx context.Context, a any) SdkResult {
		eCtx := ctx.Value(wasm_context.WasmExecCtxKey).(wasm_context.ExecContextValue)

		session := eCtx.IOSession()
		return result.Map(
			eCtx.GetEnv(),
			// data,
			func(s string) SdkResultStruct {
				gas := session.End()

				// fmt.Println("system.getEnv", envArg, s, gas)
				return SdkResultStruct{
					Result: s,
					Gas:    gas,
				}
			},
		)
	},

	//Gets current balance of an account
	"hive.get_balance": func(ctx context.Context, arg1 any, arg2 any) SdkResult {
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
	//Pulls token balance from user transction
	"hive.draw": func(ctx context.Context, arg1 any, arg2 any) SdkResult {
		eCtx := ctx.Value(wasm_context.WasmExecCtxKey).(wasm_context.ExecContextValue)
		amountString, ok := arg1.(string)
		if !ok {
			return ErrInvalidArgument
		}
		amount, err := strconv.ParseInt(amountString, 10, 64)
		if err != nil {
			return result.Err[SdkResultStruct](errors.Join(fmt.Errorf(contracts.SDK_ERROR), err))
		}
		asset, ok := arg2.(string)
		if !ok {
			return ErrInvalidArgument
		}

		return result.Map(
			eCtx.PullBalance(amount, asset),
			func(struct{}) SdkResultStruct {
				return SdkResultStruct{
					Gas: params.CYCLE_GAS_PER_RC,
				}
			},
		)
	},
	//Transfer tokens owned by contract to another user or
	"hive.transfer": func(ctx context.Context, arg1 any, arg2 any, arg3 any) SdkResult {
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
		if amount < 0 {
			return result.Err[SdkResultStruct](errors.Join(fmt.Errorf(contracts.SDK_ERROR), fmt.Errorf("amount cannot be negative")))
		}
		if err != nil {
			return result.Err[SdkResultStruct](errors.Join(fmt.Errorf(contracts.SDK_ERROR), err))
		}
		asset, ok := arg3.(string)
		if !ok {
			return ErrInvalidArgument
		}

		return result.Map(
			eCtx.SendBalance(to, amount, asset),
			func(struct{}) SdkResultStruct {
				return SdkResultStruct{
					Gas: params.CYCLE_GAS_PER_RC,
				}
			},
		)
	},
	//Triggers withdrawal of tokens owned by contract
	"hive.withdraw": func(ctx context.Context, arg1 any, arg2 any, arg3 any) SdkResult {
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
		if amount < 0 {
			return result.Err[SdkResultStruct](errors.Join(fmt.Errorf(contracts.SDK_ERROR), fmt.Errorf("amount cannot be negative")))
		}
		if err != nil {
			return result.Err[SdkResultStruct](errors.Join(fmt.Errorf(contracts.SDK_ERROR), err))
		}
		asset, ok := arg3.(string)
		if !ok {
			return ErrInvalidArgument
		}

		return result.Map(
			eCtx.WithdrawBalance(to, amount, asset),
			func(struct{}) SdkResultStruct {
				return SdkResultStruct{
					Gas: 5 * params.CYCLE_GAS_PER_RC,
				}
			},
		)
	},
	//Intercontract read
	"contracts.read": func(ctx context.Context, arg1 any, arg2 any) SdkResult {
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
			func(r string) SdkResultStruct {
				return SdkResultStruct{
					Result: r,
					Gas:    session.End(),
				}
			},
		)
	},
	//Intercontract call
	"contracts.call": func(ctx context.Context, arg1 any, arg2 any, arg3 any, arg4 any) SdkResult {
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
	"tss.create_key": func(ctx context.Context, arg1 any, arg2 any) SdkResult {
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

		res := eCtx.TssCreateKey(keyName, keyType)

		if res.IsOk() {
			return result.Ok(SdkResultStruct{
				Result: res.Unwrap(),

				//TODO: Pull atleast 100 HBD from caller wait in order to create key
				Gas: 1_000_000,
			})
		} else {
			err := res.UnwrapErr()

			return result.Err[SdkResultStruct](err)
		}
	},
	"tss.sign_key": func(ctx context.Context, arg1 any, arg2 any) SdkResult {
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

		return result.Ok(SdkResultStruct{
			Result: res.Unwrap(),
			Gas:    100_000,
		})
	},
	"tss.get_key": func(ctx context.Context, arg1 any) SdkResult {
		keyId, ok := arg1.(string)
		if !ok {
			return ErrInvalidArgument
		}
		eCtx := ctx.Value(wasm_context.WasmExecCtxKey).(wasm_context.ExecContextValue)

		res := eCtx.TssGetKey(keyId)

		return result.Ok(SdkResultStruct{
			Result: res.Unwrap(),
			Gas:    10_000,
		})
	},
	//Links contract for writes in the future
	//It is required to link a contract first to allow for VM loading
	//In the future, links can be dynamically set within the sending TX itself to ensure proper gas is allocated
	//..and vm is loaded properly
	// 	"ic.link": func(ctx context.Context, a any) SdkResult {
	// 		/*eCtx :*/ _ = ctx.Value(wasm_context.WasmExecCtxKey).(wasm_context.ExecContextValue)
	// 		return ErrUnimplemented
	// 	},
	// 	//Unlinks intercontract call
	// 	"ic.unlink": func(ctx context.Context, a any) SdkResult {
	// 		/*eCtx :*/ _ = ctx.Value(wasm_context.WasmExecCtxKey).(wasm_context.ExecContextValue)
	// 		return ErrUnimplemented
	// 	},
}
