package main

import (
	"call-tss/sdk"
	_ "call-tss/sdk" // ensure sdk is imported
	"encoding/hex"
	"strconv"
	"strings"

	. "call-tss/contract/params"

	"github.com/CosmWasm/tinyjson"
)

// setString stores a contract-state value. Input is "key,value" (matching the
// go_wasm test contract convention). Used by the node-wide regression test to
// exercise contract state writes against a freshly-built (non-stale) wasm.
//
//go:wasmexport setString
func setString(input *string) *string {
	parts := strings.SplitN(*input, ",", 2)
	if len(parts) != 2 {
		out := "invalid input, expected key,value"
		return &out
	}
	sdk.StateSetObject(parts[0], parts[1])
	out := "0"
	return &out
}

// getString reads a contract-state value by key.
//
//go:wasmexport getString
func getString(input *string) *string {
	return sdk.StateGetObject(*input)
}

// clearString deletes a contract-state value by key.
//
//go:wasmexport clearString
func clearString(input *string) *string {
	sdk.StateDeleteObject(*input)
	out := "0"
	return &out
}

// setThenAbort writes a state key then aborts. Used by the try/catch devnet test
// to prove a try-call rolls back the callee's state when it fails. Input "key,value".
//
//go:wasmexport setThenAbort
func setThenAbort(input *string) *string {
	parts := strings.SplitN(*input, ",", 2)
	if len(parts) != 2 {
		sdk.Abort("invalid input, expected key,value")
	}
	sdk.StateSetObject(parts[0], parts[1])
	sdk.Abort("intentional abort after write")
	out := "unreachable"
	return &out
}

// tryThenSet does a try/catch call to a callee, then writes its OWN state marker
// to prove it kept running after a caught failure. Returns the raw try outcome
// JSON. Input (";"-separated): "calleeId;method;callPayload;markerKey;markerVal".
//
//go:wasmexport tryThenSet
func tryThenSet(input *string) *string {
	parts := strings.Split(*input, ";")
	if len(parts) < 5 {
		sdk.Abort("invalid input")
	}
	outcome := sdk.TryContractCall(parts[0], parts[1], parts[2])
	sdk.StateSetObject(parts[3], parts[4])
	if outcome == nil {
		out := "nil-outcome"
		return &out
	}
	return outcome
}

// tryCallAs invokes sdk.ContractCallAs(target, "setString", "key,val",
// effectiveCaller) so a devnet test can exercise the call_as gate
// without going through the full IS-login attestation pipeline. Input
// is ";"-separated: "targetContractId;effectiveCallerDID;key;val".
//
// The contract is wired as the "forwarder" in a dash-mapping-contract,
// sysconfig points magi at that mapping, and this contract's id ends
// up in resolveTrustedForwarders's allow-list. When the gate is open,
// the call_as succeeds and the target's state[key] = val. When the
// gate is closed (sysconfig cleared, mapping unset, etc.) the call_as
// aborts and the surrounding wasmexport aborts too — magi records the
// contract output with err="sdk_error" and the failure message in
// errMsg.
//
// Used by TestIsLoginEmergencyRevokeViaSysconfigClear.
//
//go:wasmexport tryCallAs
func tryCallAs(input *string) *string {
	parts := strings.Split(*input, ";")
	if len(parts) != 4 {
		sdk.Abort("tryCallAs: expected 4 ';'-separated fields (targetId;effectiveCallerDID;key;val), got " + strconv.Itoa(len(parts)))
	}
	targetId := parts[0]
	effectiveCaller := parts[1]
	key := parts[2]
	val := parts[3]
	res := sdk.ContractCallAs(targetId, "setString", key+","+val, effectiveCaller)
	if res == nil {
		out := "nil-result"
		return &out
	}
	return res
}

//go:wasmexport tssCreate
func tssCreate(input *string) *string {
	var args Params
	if err := tinyjson.Unmarshal([]byte(*input), &args); err != nil {
		out := "invalid json: " + err.Error()
		return &out
	}
	pubkey := sdk.TssCreateKey(args.KeyName, "ecdsa", args.Epochs)
	sdk.Log("created key: " + pubkey)
	out := "0"
	return &out
}

//go:wasmexport tssSign
func tssSign(input *string) *string {
	var args Params
	if err := tinyjson.Unmarshal([]byte(*input), &args); err != nil {
		out := "invalid json: " + err.Error()
		return &out
	}
	msg, err := hex.DecodeString(args.MsgHex)
	if err != nil {
		out := "invalid hex: " + err.Error()
		return &out
	}
	if len(msg) != 32 {
		out := "invalid length expected 32 bytes got " + strconv.Itoa(len(msg))
		return &out
	}
	sdk.TssSignKey(args.KeyName, msg)
	sdk.Log("sign requested for key: " + args.KeyName)
	out := "0"
	return &out
}

//go:wasmexport tssRenew
func tssRenew(input *string) *string {
	var args Params
	if err := tinyjson.Unmarshal([]byte(*input), &args); err != nil {
		out := "invalid json: " + err.Error()
		return &out
	}
	sdk.TssRenewKey(args.KeyName, args.Epochs)
	out := "0"
	return &out
}
