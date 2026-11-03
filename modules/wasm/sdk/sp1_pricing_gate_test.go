package sdk

import (
	"context"
	"math/big"
	"strings"
	"testing"

	"vsc-node/modules/common/params"
	wasm_context "vsc-node/modules/wasm/context"

	"github.com/JustinKnueppel/go-result"
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

// SP-0d gate: before 0.13.0 an amount past int64 is refused with the exact
// error an older binary gives (consensus), and never reaches the applier;
// from 0.13.0 it is passed as a big integer and answered in big amounts.
type bigSwapCtx struct {
	wasm_context.ExecContextValue
	big  bool
	got  *wasm_context.PendulumSwapFeeArgs
	sess fakeIOSession
}

type fakeIOSession struct{}

func (fakeIOSession) End() uint { return 0 }

func (c *bigSwapCtx) BigSwapAmountsActive() bool        { return c.big }
func (c *bigSwapCtx) IOSession() wasm_context.IOSession { return c.sess }
func (c *bigSwapCtx) PendulumApplySwapFees(a wasm_context.PendulumSwapFeeArgs) result.Result[wasm_context.PendulumSwapFeeResult] {
	c.got = &a
	if a.XBig == nil {
		return result.Ok(wasm_context.PendulumSwapFeeResult{UserOutput: 7})
	}
	one := big.NewInt(1)
	huge, _ := new(big.Int).SetString("99000000000000000000", 10)
	return result.Ok(wasm_context.PendulumSwapFeeResult{Big: &wasm_context.PendulumSwapFeeBig{
		UserOutput: huge, NewXReserve: one, NewYReserve: one, NetworkCreditOutput: one, LpShareOutput: one, NodeShareOutput: one}})
}

func TestPendulumSwapFees_BigAmountsFollowTheGate(t *testing.T) {
	fn := SdkNamespaces["system"]["pendulum_apply_swap_fees"].(func(context.Context, any) SdkResult)
	payload := `{"asset_in":"hbd","asset_out":"eth","x":"1000000","x_reserve":"100000000","y_reserve":"100000000000000000000"}`

	old := &bigSwapCtx{}
	res := fn(context.WithValue(context.Background(), wasm_context.WasmExecCtxKey, wasm_context.ExecContextValue(old)), payload)
	if !res.IsErr() || res.UnwrapErr().Error() != ErrInvalidArgument.UnwrapErr().Error() || old.got != nil {
		t.Fatalf("below 0.13: %v (reached applier: %v), want the old invalid-argument refusal", res, old.got != nil)
	}

	c := &bigSwapCtx{big: true}
	res = fn(context.WithValue(context.Background(), wasm_context.WasmExecCtxKey, wasm_context.ExecContextValue(c)), payload)
	if res.IsErr() {
		t.Fatalf("0.13: %v", res.UnwrapErr())
	}
	if c.got == nil || c.got.YReserveBig == nil || c.got.YReserveBig.String() != "100000000000000000000" || c.got.YReserve != 0 {
		t.Fatalf("0.13: applier got %+v, want the big reserve", c.got)
	}
	if !strings.Contains(res.Unwrap().Result, `"user_output":"99000000000000000000"`) {
		t.Fatalf("0.13: big output not passed back: %s", res.Unwrap().Result)
	}

	// Within int64 and below 0.13: unchanged path (int64 fields, no Big).
	small := `{"asset_in":"hbd","asset_out":"eth","x":"1","x_reserve":"2","y_reserve":"3"}`
	c2 := &bigSwapCtx{}
	res = fn(context.WithValue(context.Background(), wasm_context.WasmExecCtxKey, wasm_context.ExecContextValue(c2)), small)
	if res.IsErr() || c2.got == nil || c2.got.XBig != nil || c2.got.YReserve != 3 {
		t.Fatalf("below 0.13 within int64: %v %+v", res, c2.got)
	}
}
