package sdk

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	wasm_context "vsc-node/modules/wasm/context"
	wasm_types "vsc-node/modules/wasm/types"

	"github.com/JustinKnueppel/go-result"
)

type SdkResultStruct = wasm_types.WasmResultStruct
type SdkResult = result.Result[SdkResultStruct]

var (
	ErrInvalidArgument = result.Err[SdkResultStruct](fmt.Errorf("invalid argument"))
	ErrUnimplemented   = result.Err[SdkResultStruct](fmt.Errorf("unimplemented"))
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

		fmt.Println("console.log(s):", s)
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
			return result.Err[SdkResultStruct](fmt.Errorf("INVALID_CALL"))
		}
	},
	"system.get_env_key": func(ctx context.Context, a any) SdkResult {
		eCtx := ctx.Value(wasm_context.WasmExecCtxKey).(wasm_context.ExecContextValue)
		envArg, ok := a.(string)
		if !ok {
			return ErrInvalidArgument
		}

		fmt.Println("Let's get env", envArg)
		res := eCtx.EnvVar(envArg)

		fmt.Println("system.getEnv 50", envArg, eCtx.EnvVar(envArg), res.Unwrap())

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

	//Gets current balance of contract account or tag
	//Cannot be used to get balance of other accounts (or generally shouldn"t)
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
			Gas:    100_000,
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
			return result.Err[SdkResultStruct](err)
		}
		asset, ok := arg2.(string)
		if !ok {
			return ErrInvalidArgument
		}

		return result.Map(
			eCtx.PullBalance(amount, asset),
			func(struct{}) SdkResultStruct {
				return SdkResultStruct{
					Gas: 1_000_000,
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
			return result.Err[SdkResultStruct](fmt.Errorf("amount cannot be negative"))
		}
		if err != nil {
			return result.Err[SdkResultStruct](err)
		}
		asset, ok := arg3.(string)
		if !ok {
			return ErrInvalidArgument
		}

		return result.Map(
			eCtx.SendBalance(to, amount, asset),
			func(struct{}) SdkResultStruct {
				return SdkResultStruct{
					Gas: 1_000_000,
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
			return result.Err[SdkResultStruct](fmt.Errorf("amount cannot be negative"))
		}
		if err != nil {
			return result.Err[SdkResultStruct](err)
		}
		asset, ok := arg3.(string)
		if !ok {
			return ErrInvalidArgument
		}

		return result.Map(
			eCtx.WithdrawBalance(to, amount, asset),
			func(struct{}) SdkResultStruct {
				return SdkResultStruct{
					Gas: 10_000_000,
				}
			},
		)
	},
	//Intercontract read
	"contracts.read": func(ctx context.Context, a any) SdkResult {
		/*eCtx :*/ _ = ctx.Value(wasm_context.WasmExecCtxKey).(wasm_context.ExecContextValue)
		// if(this.state[contractId]) {
		// 	state := this.state[contractId]
		// 	{stateCache} := state
		// 	result := stateCache.get(key)
		// 	return result
		// } else {
		// 	contractOutput := this.contractOuputs.findOne({
		// 		contractId: contractId,
		// 		anchored_height: {
		// 			$lte: this.op.block_height
		// 		}
		// 	}, {
		// 		//Latest height
		// 		sort: {
		// 			anchored_height: -1
		// 		}
		// 	})
		// 	console.log("contractOutput", contractOutput)

		// 	stateCache := new StateCache(contractOutput.state_merkle)
		// 	this.state[contractId] = {
		// 		stateCache,
		// 		stateCid: contractOutput.state_merkle
		// 	}
		// 	result := stateCache.get(key)
		// 	return result
		// }
		return ErrUnimplemented
	},
	//Intercontract write
	"contracts.call": func(ctx context.Context, a any) SdkResult {
		/*eCtx :*/ _ = ctx.Value(wasm_context.WasmExecCtxKey).(wasm_context.ExecContextValue)
		return ErrUnimplemented
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
