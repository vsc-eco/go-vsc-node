package main

import (
	"encoding/hex"
	"strconv"
	"strings"
	"vsc-node/modules/wasm/e2e/go_wasm/sdk"

	"github.com/CosmWasm/tinyjson"
)

func main() {

}

//Reference test functions

// - Set & Get Object
// - get_env

//go:wasmexport test1
func TestStatePut(arg *string) *string {
	sdk.Log("hit 1")
	env := sdk.GetEnv()
	sdk.Log("Contract ID:" + env.ContractId)

	sdk.StateSetObject("hello", "world")
	sdk.StateSetObject("key-1", "value-1")

	value := sdk.StateGetObject("hello")

	if *value != "world" {
		sdk.Abort("invalid state access")
	}

	sdk.Log("hello world")

	bytes, _ := tinyjson.Marshal(env)

	ret := "env: " + string(bytes)
	return &ret
}

// - Access Saved State
// - Delete a saved key
//
//go:wasmexport test2
func TestStateGetModify(arg *string) *string {
	ret := "success"

	sdk.Log("step 1")
	value := sdk.StateGetObject("key-1")

	sdk.Log("step 2")
	if *value != "value-1" {
		sdk.Abort("invalid state access")
	}
	sdk.Log("step 3")
	sdk.StateDeleteObject("key-1")

	value = sdk.StateGetObject("key-1")

	sdk.Log("step 4 data:" + *value)
	if *value != "" {
		sdk.Abort("invalid state access")
	}

	return &ret
}

// - Draw user balance to contract
// - Verify pulled balance
//
//go:wasmexport test3
func TestTokenDraw(arg *string) *string {
	env := sdk.GetEnv()

	sdk.Log("test3: step 1")
	sdk.HiveDraw(10, sdk.AssetHive)
	sdk.Log("test3: step 2")
	bal := sdk.GetBalance(sdk.Address("contract:"+env.ContractId), sdk.AssetHive)
	sdk.Log("test3: step 3" + strconv.Itoa(int(bal)))
	if bal != 10 {
		sdk.Abort("incorrect balance pull")
	}

	sdk.Log("test3: step 4")
	balStr := strconv.FormatInt(bal, 10)
	ret := "bal: " + balStr

	return &ret
}

//go:wasmexport test4
func TestTokenSend(arg *string) *string {
	env := sdk.GetEnv()

	bal := sdk.GetBalance(sdk.Address("contract:"+env.ContractId), sdk.AssetHive)

	if bal != 10 {
		sdk.Abort("incorrect balance pull")
	}

	balStr := strconv.FormatInt(bal, 10)
	ret := "bal: " + balStr

	sdk.HiveTransfer(env.Sender.Address, 10, sdk.AssetHive)

	return &ret
}

//go:wasmexport test5
func TestMultiSend(arg *string) *string {

	sdk.HiveDraw(10, sdk.AssetHive)

	to := sdk.Address("hive:vaultec")
	sdk.HiveTransfer(to, 10, sdk.AssetHive)

	ret := "success"
	return &ret
}

// - Contract read
// - Contract call
// - Contract read
// TODO: Must be implemented
//
//go:wasmexport test6
func TestContractOps(arg *string) *string {
	ret := "hello world"

	return &ret
}

// - Contract call (reversion)
// TODO: Must be implemented
//
//go:wasmexport test7
func TestContractOps2(arg *string) *string {
	ret := "hello world"

	return &ret
}

//go:wasmexport setString
func SetString(a *string) *string {
	params := strings.Split((*a), ",")
	if len(params) < 2 {
		sdk.Abort("invalid payload")
	}
	sdk.StateSetObject(params[0], params[1])
	return a
}

//go:wasmexport getString
func GetString(a *string) *string {
	return sdk.StateGetObject(*a)
}

//go:wasmexport clearString
func ClearString(a *string) *string {
	sdk.StateDeleteObject(*a)
	return nil
}

//go:wasmexport dumpEnv
func DumpEnv(a *string) *string {
	env := sdk.GetEnv()
	envJson, err := tinyjson.Marshal(env)
	if err != nil {
		sdk.Abort("failed to stringify env vars")
	}
	ret := string(envJson)
	sdk.Log("Dump completed")
	return &ret
}

//go:wasmexport dumpEnvKey
func DumpEnvKey(a *string) *string {
	return sdk.GetEnvKey(*a)
}

//go:wasmexport getHiveBalance
func GetHiveBalance(a *string) *string {
	ret := strconv.FormatInt(sdk.GetBalance(sdk.Address(*a), sdk.AssetHive), 10)
	return &ret
}

//go:wasmexport getHbdBalance
func GetHbdBalance(a *string) *string {
	ret := strconv.FormatInt(sdk.GetBalance(sdk.Address(*a), sdk.AssetHbd), 10)
	return &ret
}

//go:wasmexport getHiveConsBalance
func GetHiveConsBalance(a *string) *string {
	ret := strconv.FormatInt(sdk.GetBalance(sdk.Address(*a), sdk.AssetHiveCons), 10)
	return &ret
}

//go:wasmexport drawHive
func DrawHive(a *string) *string {
	amt, err := strconv.ParseInt(*a, 10, 64)
	if err != nil {
		sdk.Abort("invalid amount")
	}
	sdk.HiveDraw(amt, sdk.AssetHive)
	return nil
}

//go:wasmexport drawHbd
func DrawHbd(a *string) *string {
	amt, err := strconv.ParseInt(*a, 10, 64)
	if err != nil {
		sdk.Abort("invalid amount")
	}
	sdk.HiveDraw(amt, sdk.AssetHbd)
	return nil
}

//go:wasmexport transferHive
func TransferHive(a *string) *string {
	params := strings.Split((*a), ",")
	if len(params) < 2 {
		sdk.Abort("invalid payload")
	}
	amt, err := strconv.ParseInt(params[1], 10, 64)
	if err != nil {
		sdk.Abort("invalid amount")
	}
	sdk.HiveTransfer(sdk.Address(params[0]), amt, sdk.AssetHive)
	return nil
}

//go:wasmexport transferHbd
func TransferHbd(a *string) *string {
	params := strings.Split((*a), ",")
	if len(params) < 2 {
		sdk.Abort("invalid payload")
	}
	amt, err := strconv.ParseInt(params[1], 10, 64)
	if err != nil {
		sdk.Abort("invalid amount")
	}
	sdk.HiveTransfer(sdk.Address(params[0]), amt, sdk.AssetHbd)
	return nil
}

//go:wasmexport withdrawHive
func WithdrawHive(a *string) *string {
	params := strings.Split((*a), ",")
	if len(params) < 2 {
		sdk.Abort("invalid payload")
	}
	amt, err := strconv.ParseInt(params[1], 10, 64)
	if err != nil {
		sdk.Abort("invalid amount")
	}
	sdk.HiveWithdraw(sdk.Address(params[0]), amt, sdk.AssetHive)
	return nil
}

//go:wasmexport withdrawHbd
func WithdrawHbd(a *string) *string {
	params := strings.Split((*a), ",")
	if len(params) < 2 {
		sdk.Abort("invalid payload")
	}
	amt, err := strconv.ParseInt(params[1], 10, 64)
	if err != nil {
		sdk.Abort("invalid amount")
	}
	sdk.HiveWithdraw(sdk.Address(params[0]), amt, sdk.AssetHbd)
	return nil
}

//go:wasmexport abortMe
func AbortMe(a *string) *string {
	sdk.Abort(*a)
	ret := "Task failed successfully"
	return &ret
}

//go:wasmexport revertMe
func RevertMe(a *string) *string {
	sdk.Revert(*a, "symbol_here")
	ret := "Task failed successfully"
	return &ret
}

//go:wasmexport contractGetString
func ContractGetString(a *string) *string {
	params := strings.Split((*a), ",")
	if len(params) < 2 {
		sdk.Revert("invalid payload", "invalid_payload")
	}
	return sdk.ContractStateGet(params[0], params[1])
}

//go:wasmexport contractCall
func ContractCall(a *string) *string {
	params := strings.Split((*a), ",")
	if len(params) < 3 {
		sdk.Revert("invalid payload", "invalid_payload")
	}
	return sdk.ContractCall(params[0], params[1], params[2], &sdk.ContractCallOptions{
		Intents: []sdk.Intent{{
			Type: "transfer.allow",
			Args: map[string]string{
				"token": "hive",
				"limit": "1.000",
			},
		}},
	})
}

//go:wasmexport infiniteRecursion
func InfRecursion(a *string) *string {
	contractId := sdk.GetEnvKey("contract.id")
	return sdk.ContractCall(*contractId, "infiniteRecursion", "a", nil)
}

//go:wasmexport createKey
func CreateKey(a *string) *string {
	status := sdk.TssCreateKey("main", "eddsa")

	return &status
}

//go:wasmexport getKey
func GetKey(a *string) *string {
	ret := "result="

	keyData := sdk.TssGetKey("main")

	ret = ret + keyData
	return &ret
}

//go:wasmexport signKey
func SignKey(a *string) *string {
	msg, _ := hex.DecodeString("89d7d1a68f8edd0cc1f961dce816422055d1ab69a0623954b834c95c1cdd7ed0")

	sdk.TssSignKey("main", msg)

	ret := ""
	return &ret
}
