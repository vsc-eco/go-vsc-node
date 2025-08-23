package main

import (
	"encoding/json"
	"strconv"
	"vsc-node/modules/wasm/e2e/go_wasm/sdk"
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

	bytes, _ := json.Marshal(env)

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
