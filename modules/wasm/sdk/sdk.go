package sdk

import (
	"context"
	"encoding/json"
	"fmt"
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

var sdkModuleRef *map[string]func(context.Context, any) SdkResult

func init() {
	sdkModuleRef = &SdkModule
}

var SdkModule = map[string]func(context.Context, any) SdkResult{
	"console.log": func(ctx context.Context, a any) SdkResult {
		eCtx := ctx.Value(wasm_context.WasmExecCtxKey).(wasm_context.ExecContextValue)
		s, ok := a.(string)
		if !ok {
			return ErrInvalidArgument
		}
		session := eCtx.IOSession()
		eCtx.Log(s)
		gas := session.End()
		return result.Ok(SdkResultStruct{
			Gas: gas,
		})
	},
	"console.logNumber": func(ctx context.Context, a any) SdkResult {
		return (*sdkModuleRef)["console.log"](ctx, fmt.Sprint(a))
	},
	"console.logBool": func(ctx context.Context, a any) SdkResult {
		return (*sdkModuleRef)["console.log"](ctx, fmt.Sprint(a))
	},
	"db.setObject": func(ctx context.Context, a any) SdkResult {
		eCtx := ctx.Value(wasm_context.WasmExecCtxKey).(wasm_context.ExecContextValue)
		args, ok := a.([]string)
		if !ok || len(args) != 2 {
			return ErrInvalidArgument
		}
		key := args[0]
		val := args[1]

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
	"db.getObject": func(ctx context.Context, a any) SdkResult {
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
	"db.delObject": func(ctx context.Context, a any) SdkResult {
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
	"system.call": func(ctx context.Context, a any) SdkResult {
		/*eCtx :*/ _ = ctx.Value(wasm_context.WasmExecCtxKey).(wasm_context.ExecContextValue)
		args, ok := a.([]string)
		if !ok || len(args) != 2 {
			return ErrInvalidArgument
		}
		callArg := args[0]
		var valArg struct {
			Arg0 string `json:"arg0"`
		}
		err := json.Unmarshal([]byte(args[1]), &valArg)

		f, ok := (*sdkModuleRef)[callArg]
		if ok {
			return result.And(
				result.Err[any](err),
				f(ctx, valArg.Arg0),
			)
		} else {
			return result.Err[SdkResultStruct](fmt.Errorf("INVALID_CALL"))
		}
	},
	"system.getEnv": func(ctx context.Context, a any) SdkResult {
		eCtx := ctx.Value(wasm_context.WasmExecCtxKey).(wasm_context.ExecContextValue)
		envArg, ok := a.(string)
		if !ok {
			return ErrInvalidArgument
		}

		session := eCtx.IOSession()
		return result.Map(
			eCtx.EnvVar(envArg),
			func(s string) SdkResultStruct {
				gas := session.End()
				return SdkResultStruct{
					Result: s,
					Gas:    gas,
				}
			},
		)
	},
	"crypto.sha256": func(ctx context.Context, a any) SdkResult {
		/*eCtx :*/ _ = ctx.Value(wasm_context.WasmExecCtxKey).(wasm_context.ExecContextValue)
		return ErrUnimplemented
		// return sha256(Buffer.from(value, "hex")).toString("hex")
	},
	"crypto.ripemd160": func(ctx context.Context, a any) SdkResult {
		/*eCtx :*/ _ = ctx.Value(wasm_context.WasmExecCtxKey).(wasm_context.ExecContextValue)
		return ErrUnimplemented
		// return ripemd160(Buffer.from(value, "hex")).toString("hex")
	},
	//Gets current balance of contract account or tag
	//Cannot be used to get balance of other accounts (or generally shouldn"t)
	"hive.getbalance": func(ctx context.Context, a any) SdkResult {
		/*eCtx :*/ _ = ctx.Value(wasm_context.WasmExecCtxKey).(wasm_context.ExecContextValue)
		// args: :{
		// 	account: string
		// 	tag: string
		// } = JSON.parse(value)
		// snapshot := this.getBalanceSnapshot(`${args.account}${args.tag  "#" + args.tag.replace("#", "") : ""}`, block_height)

		// return result.Ok(snapshot.tokens)
		return ErrUnimplemented
	},
	//Pulls token balance from user transction
	"hive.draw": func(ctx context.Context, a any) SdkResult {
		/*eCtx :*/ _ = ctx.Value(wasm_context.WasmExecCtxKey).(wasm_context.ExecContextValue)
		// const args:{
		// 	from: string
		// 	amount: number
		// 	asset: "HIVE" | "HBD"
		// 	tag: string
		// } = JSON.parse(value)
		// snapshot := this.getBalanceSnapshot(args.from, block_height)

		//Total amount drawn from ledgerStack during this execution
		// totalAmountDrawn := Math.abs(this.ledgerStack.filter(sift({
		// 	owner: args.from,
		// 	to: contract_id,
		// 	unit: args.asset
		// })).reduce((acc, cur) => acc + cur.amt, 0))

		// allowedByIntent := this.verifyIntent("hive.allow_transfer", {
		// 	token: {
		// 		$eq: args.asset.toLowerCase()
		// 	},
		// 	limit: {
		// 		$gte: args.amount + totalAmountDrawn
		// 	}
		// })

		// if(!allowedByIntent) {
		// 	return {
		// 		result: "MISSING_INTENT_HEADER"
		// 	}
		// }

		// if(snapshot.tokens[args.asset] >= args.amount) {
		// 	this.applyLedgerOp({
		// 		t: EventOpType["ledger:transfer"],
		// 		owner: args.from,
		// 		amt: -args.amount,
		// 		tk: args.asset
		// 	})
		// 	this.applyLedgerOp({
		// 		t: EventOpType["ledger:transfer"],
		// 		//Tag using contract address #tag
		// 		owner: args.tag  `${contract_id}#${args.tag}` : contract_id,
		// 		amt: args.amount,
		// 		tk: args.asset
		// 	})

		// 	return {
		// 		result: "SUCCESS"
		// 	}
		// } else {
		// 	return {
		// 		result: "INSUFFICIENT_FUNDS"
		// 	}
		// }
		return ErrUnimplemented
	},
	//Transfer tokens owned by contract to another user or
	"hive.transfer": func(ctx context.Context, a any) SdkResult {
		/*eCtx :*/ _ = ctx.Value(wasm_context.WasmExecCtxKey).(wasm_context.ExecContextValue)
		// const args: {
		// 	//$self#tag
		// 	dest: string
		// 	//Transfer tag
		// 	from_tag: string
		// 	memo: string
		// 	amount: number
		// 	asset: "HIVE" | "HBD"
		// } = JSON.parse(value)
		// normalizedFrom := args.from_tag  `${contract_id}#${args.from_tag}` : contract_id
		// var normalizedDest any

		// if(!["HIVE", "HBD"].includes(args.asset)) {
		// 	return {
		// 		result: "INVALID_ASSET"
		// 	}
		// }

		// if args.dest == "$self" {
		// const [, tag] = args.dest.split("#")
		// normalizedDest = tag  `${contract_id}#${tag}` : contract_id
		// } else {
		// if args.dest.startsWith("did:") || args.dest.startsWith("hive:") {
		// normalizedDest = args.dest
		// } else {
		// return {
		// 	result: "INVALID_DEST"
		// }
		// }
		// }
		// snapshot := this.getBalanceSnapshot(normalizedFrom, block_height)
		// if snapshot.tokens[args.asset] >= args.amount {
		// this.applyLedgerOp({
		// 	t: EventOpType["ledger:transfer"],
		// 	owner: normalizedFrom,
		// 	amt: -args.amount,
		// 	tk: args.asset
		// })

		// this.applyLedgerOp({
		// 	t: EventOpType["ledger:transfer"],
		// 	owner: normalizedDest,
		// 	amt: args.amount,
		// 	tk: args.asset,
		// 	//Memo will always be in last op to save space. Indexing makes it easier to search for memos
		// 	...(args.memo  {memo: args.memo} : {})
		// })

		// return {
		// 	result: "SUCCESS"
		// }
		// } else {
		// return {
		// 	result: "INSUFFICIENT_FUNDS"
		// }
		// }

		return ErrUnimplemented
	},
	//Triggers withdrawal of tokens owned by contract
	"hive.withdraw": func(ctx context.Context, a any) SdkResult {
		/*eCtx :*/ _ = ctx.Value(wasm_context.WasmExecCtxKey).(wasm_context.ExecContextValue)
		// const args:{
		// 	dest: string
		// 	from_tag: string
		// 	memo: string
		// 	amount: number
		// 	asset: "HIVE" | "HBD"
		// } = JSON.parse(value)
		// normalizedFrom := args.from_tag  `${contract_id}#${args.from_tag}` : contract_id
		// var normalizedDest any

		// if(!["HIVE", "HBD"].includes(args.asset)) {
		// 	return {
		// 		result: "INVALID_ASSET"
		// 	}
		// }

		// if args.dest.startsWith("hive:") {
		// normalizedDest = args.dest
		// } else {
		// return {
		// 	result: "INVALID_DEST"
		// }
		// }

		// snapshot := this.getBalanceSnapshot(normalizedFrom, block_height)
		// console.log("snapshot result", snapshot)

		// if snapshot.tokens[args.asset] >= args.amount {
		// this.applyLedgerOp({
		// 	t: EventOpType["ledger:withdraw"],
		// 	owner: normalizedFrom,
		// 	amt: -args.amount,
		// 	tk: args.asset,
		// })
		// this.applyLedgerOp({
		// 	t: EventOpType["ledger:withdraw"],
		// 	owner: `#withdrawto=${normalizedDest}`,
		// 	amt: args.amount,
		// 	tk: args.asset,
		// 	...(args.memo  {memo: args.memo} : {})
		// })
		// console.log(this.ledgerStack)
		// return {
		// 	result: "SUCCESS"
		// }
		// } else {
		// return {
		// 	result: "INSUFFICIENT_FUNDS"
		// }
		// }
		return ErrUnimplemented
	},
	//Intercontract read
	"ic.read": func(ctx context.Context, a any) SdkResult {
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
	"ic.call": func(ctx context.Context, a any) SdkResult {
		/*eCtx :*/ _ = ctx.Value(wasm_context.WasmExecCtxKey).(wasm_context.ExecContextValue)
		return ErrUnimplemented
	},
	//Links contract for writes in the future
	//It is required to link a contract first to allow for VM loading
	//In the future, links can be dynamically set within the sending TX itself to ensure proper gas is allocated
	//..and vm is loaded properly
	"ic.link": func(ctx context.Context, a any) SdkResult {
		/*eCtx :*/ _ = ctx.Value(wasm_context.WasmExecCtxKey).(wasm_context.ExecContextValue)
		return ErrUnimplemented
	},
	//Unlinks intercontract call
	"ic.unlink": func(ctx context.Context, a any) SdkResult {
		/*eCtx :*/ _ = ctx.Value(wasm_context.WasmExecCtxKey).(wasm_context.ExecContextValue)
		return ErrUnimplemented
	},
}
