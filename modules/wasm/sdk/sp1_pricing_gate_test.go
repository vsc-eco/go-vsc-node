package sdk

import (
	"context"
	"strings"
	"testing"

	"vsc-node/modules/common/params"
	wasm_context "vsc-node/modules/wasm/context"
)

type sp1GateCtx struct {
	wasm_context.ExecContextValue
	workPricing bool
}

func (c sp1GateCtx) Sp1WorkPricingActive() bool { return c.workPricing }

// Below consensus 0.10.0 an SP1 check keeps the legacy flat cost, so history
// re-executes unchanged; from 0.10.0 it is priced by work and a large public
// input costs more.
func TestSp1PricingFollowsTheGate(t *testing.T) {
	fn := SdkNamespaces["crypto"]["sp1_verify_groth16"].(func(context.Context, any, any, any, any, any) SdkResult)
	inputs := strings.Repeat("ab", 4096) // 4 KiB of public input
	call := func(ctx context.Context) uint {
		res := fn(ctx, "00", inputs, "00", "00", "00")
		if res.IsErr() {
			t.Fatalf("sp1 call errored: %v", res.UnwrapErr())
		}
		return res.Unwrap().Gas
	}
	withGate := func(on bool) context.Context {
		return context.WithValue(context.Background(), wasm_context.WasmExecCtxKey, wasm_context.ExecContextValue(sp1GateCtx{workPricing: on}))
	}

	legacy := uint(params.CYCLE_GAS_PER_RC * 10)
	if got := call(withGate(false)); got != legacy {
		t.Fatalf("gate off: gas %d, want the legacy flat %d", got, legacy)
	}
	if got := call(context.Background()); got != legacy {
		t.Fatalf("no execution context: gas %d, want the legacy flat %d", got, legacy)
	}
	want := sp1VerifyBaseGas + uint(4096)*sp1GasPerInputByte
	if got := call(withGate(true)); got != want {
		t.Fatalf("gate on: gas %d, want %d", got, want)
	}
}
