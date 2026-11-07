package main

import (
	"strconv"
	"strings"

	"amm-pool/sdk"
)

// amm-pool is a minimal HBD-paired pool used by the pendulum LP-floor devnet
// test. It does NOT implement a real CPMM; it only does the three things the
// pendulum pipeline observes:
//
//   - setup() publishes the pool's geometry state keys (a0n/a1n/r0/r1) so the
//     node's GeometryComputer reads this pool as HBD-paired with a chosen HBD
//     reserve. A huge r0 forces V = 2*r0 >> c*E (the under-secured regime where
//     the raw pendulum routes ~100% to nodes — exactly where the 0.2.0 LP floor
//     must cap the node share at 75%).
//   - fund() draws real HBD into the pool's balance so the applier's node-share
//     drain (a paired transfer from contract:<id> to pendulum:nodes) succeeds.
//   - swap() forwards swap inputs to system.pendulum_apply_swap_fees and stores
//     the returned split JSON under the "result" state key for the test to read
//     cross-node.

// u64BE encodes n as minimal unsigned big-endian bytes — the form
// pendulumPoolReserveReader decodes pool reserves from (big.Int.SetBytes).
func u64BE(n uint64) string {
	if n == 0 {
		return string([]byte{0})
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte(n & 0xff)}, b...)
		n >>= 8
	}
	return string(b)
}

func mustU64(s string) uint64 {
	v, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	if err != nil {
		sdk.Abort("invalid uint: " + s)
	}
	return v
}

// jsonField extracts a string-valued field from a flat JSON object without a
// full JSON parser (the pendulum result is flat with all-string values):
// `"<key>":"<value>"`.
func jsonField(js, key string) string {
	marker := "\"" + key + "\":\""
	i := strings.Index(js, marker)
	if i < 0 {
		return ""
	}
	i += len(marker)
	j := strings.Index(js[i:], "\"")
	if j < 0 {
		return ""
	}
	return js[i : i+j]
}

// setup publishes pendulum geometry. Input CSV: "<r0_hbd>,<r1_hive>" (base units).
//
//go:wasmexport setup
func setup(input *string) *string {
	parts := strings.Split(*input, ",")
	if len(parts) != 2 {
		sdk.Abort("setup expects <r0_hbd>,<r1_hive>")
	}
	sdk.StateSetObject("a0n", "hbd")
	sdk.StateSetObject("a1n", "hive")
	sdk.StateSetObject("r0", u64BE(mustU64(parts[0])))
	sdk.StateSetObject("r1", u64BE(mustU64(parts[1])))
	out := "0"
	return &out
}

// fund draws HBD from the caller's transfer.allow intent into the pool balance,
// backing the node-share drain the pendulum applier performs on swap. Input:
// HBD amount in base units (decimal string).
//
//go:wasmexport fund
func fund(input *string) *string {
	sdk.HiveDraw(strings.TrimSpace(*input), "hbd")
	// Record the pool's own HBD balance after the draw so the test can confirm
	// funding via a plain state key (contract-account balances aren't reliably
	// exposed via the account-balance GQL).
	if id := sdk.GetEnvKey("contract.id"); id != nil {
		sdk.StateSetObject("funded_hbd", sdk.HiveGetBalance("contract:"+*id, "hbd"))
	}
	out := "0"
	return &out
}

// swap forwards the pendulum swap inputs to system.pendulum_apply_swap_fees and
// records the returned split. Input is the SDK input JSON:
// {"asset_in":"hive","asset_out":"hbd","x":"..","x_reserve":"..","y_reserve":".."}.
// Stores the full result JSON under "result"; aborts (tx fails) if the host
// rejects the swap (not whitelisted / insufficient reserves / etc.).
//
//go:wasmexport swap
func swap(input *string) *string {
	res := sdk.PendulumApplySwapFees(*input)
	sdk.StateSetObject("result", res)
	// Surface the split fields as plain decimal-string state keys so the test
	// can read them cross-node without JSON-in-state ambiguity.
	sdk.StateSetObject("lp_share", jsonField(res, "lp_share_output"))
	sdk.StateSetObject("node_share", jsonField(res, "node_share_output"))
	sdk.StateSetObject("s_after", jsonField(res, "s_after_bps"))
	sdk.StateSetObject("node_bucket", jsonField(res, "node_bucket_credited_hbd"))
	return &res
}
