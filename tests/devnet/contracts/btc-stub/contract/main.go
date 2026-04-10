// Package main implements a minimal stub of the BTC mapping contract for
// devnet oracle tests. It accepts the same wire format the oracle producer
// emits (utxoAddBlocksPayload) but skips all consensus/validation logic.
//
// State layout:
//
//	"h" -> ASCII decimal block height (matches lastHeightStateKey in
//	       modules/oracle/chain/chain_relay.go so getContractBlockHeight works)
//
// Wasmexports (mirrors the real BTC mapping contract surface used by the
// oracle):
//
//	init        — set initial height (one-shot)
//	addBlocks   — increment h by len(payload.blocks)/160 (80 bytes hex = 160)
//	replaceBlocks — set h = h - depth + new headers count (rough)
//
// This contract performs ZERO validation. Its only job is to advance "h"
// so the oracle's `getContractBlockHeight` returns a sensible value and the
// producer keeps generating new relay attempts during a test.
package main

import (
	"btc-stub/sdk"
	_ "btc-stub/sdk" // ensure sdk is imported
	"strconv"

	. "btc-stub/contract/params"

	"github.com/CosmWasm/tinyjson"
)

const heightKey = "h"

// readHeight returns the contract's current stored "h" value, or 0 if unset
// or unparseable.
func readHeight() uint64 {
	v := sdk.StateGetObject(heightKey)
	if v == nil {
		return 0
	}
	h, err := strconv.ParseUint(*v, 10, 64)
	if err != nil {
		return 0
	}
	return h
}

func writeHeight(h uint64) {
	s := strconv.FormatUint(h, 10)
	sdk.StateSetObject(heightKey, s)
}

//go:wasmexport init
func InitContract(input *string) *string {
	var args InitParams
	if err := tinyjson.Unmarshal([]byte(*input), &args); err != nil {
		out := "invalid init json: " + err.Error()
		return &out
	}
	writeHeight(args.StartHeight)
	sdk.Log("btc-stub init height=" + strconv.FormatUint(args.StartHeight, 10))
	out := "0"
	return &out
}

//go:wasmexport addBlocks
func AddBlocks(input *string) *string {
	var args AddBlocksParams
	if err := tinyjson.Unmarshal([]byte(*input), &args); err != nil {
		out := "invalid addBlocks json: " + err.Error()
		return &out
	}

	// Each BTC block header is 80 bytes = 160 hex characters.
	// Count headers without parsing them — the stub doesn't care about content.
	hexLen := len(args.Blocks)
	if hexLen%160 != 0 {
		out := "blocks length not a multiple of 160 hex chars"
		return &out
	}
	count := uint64(hexLen / 160)
	if count == 0 {
		out := "no blocks supplied"
		return &out
	}

	cur := readHeight()
	newH := cur + count
	writeHeight(newH)

	sdk.Log("btc-stub addBlocks +" + strconv.FormatUint(count, 10) +
		" -> h=" + strconv.FormatUint(newH, 10))
	out := "0"
	return &out
}

//go:wasmexport replaceBlocks
func ReplaceBlocks(input *string) *string {
	// The oracle's replaceBlocks payload is the same concatenated hex string,
	// representing the canonical headers from a fork point to the new tip.
	// For the stub we treat the call as: rewind to (h - count) then increment
	// by count, leaving h unchanged. This is the simplest interpretation that
	// keeps the contract responsive without inventing reorg semantics.
	hex := *input
	if len(hex)%160 != 0 || len(hex) == 0 {
		out := "invalid replaceBlocks payload"
		return &out
	}
	sdk.Log("btc-stub replaceBlocks no-op count=" +
		strconv.Itoa(len(hex)/160))
	out := "0"
	return &out
}
