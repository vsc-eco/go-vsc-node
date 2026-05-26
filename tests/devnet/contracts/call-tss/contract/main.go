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
