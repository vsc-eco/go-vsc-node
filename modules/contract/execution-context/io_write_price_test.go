package contract_execution_context_test

import (
	"strings"
	"testing"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/common/params"
	contract_execution_context "vsc-node/modules/contract/execution-context"
)

// ioCtx builds an execution context for contractId over a fresh harness call
// session, with the given options. The harness's data layer is closed when the
// test ends.
func ioCtx(t *testing.T, contractId string, opts ...contract_execution_context.Option) contract_execution_context.ContractExecutionContext {
	t.Helper()
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	return contract_execution_context.New(
		contract_execution_context.Environment{ContractId: contractId},
		1_000_000, 0, 1_000_000*params.CYCLE_GAS_PER_RC, nil, ct.CallSession, 0, opts...,
	)
}

// The price of a new stored byte is the one WithIoWriteRcCost carries. Without
// the option a context charges the original 19, which is what every node
// charged up to consensus 0.15.0: a call site that forgets the option keeps the
// old price, it never falls to zero.
func TestNewStoredBytesAreChargedTheContextsWritePrice(t *testing.T) {
	const key = "k"
	value := strings.Repeat("x", 1000)

	cases := []struct {
		name string
		opts []contract_execution_context.Option
		want int // RC per new byte
	}{
		{"no option: the original price", nil, 19},
		{"the original price passed explicitly", []contract_execution_context.Option{contract_execution_context.WithIoWriteRcCost(params.WRITE_IO_GAS_RC_COST)}, 19},
		{"the 0.15.0 price", []contract_execution_context.Option{contract_execution_context.WithIoWriteRcCost(params.WRITE_IO_GAS_RC_COST_V0_15)}, 1},
		{"zero is ignored", []contract_execution_context.Option{contract_execution_context.WithIoWriteRcCost(0)}, 19},
		{"a negative price is ignored", []contract_execution_context.Option{contract_execution_context.WithIoWriteRcCost(-5)}, 19},
		{"zero after a real price keeps the real price", []contract_execution_context.Option{
			contract_execution_context.WithIoWriteRcCost(1), contract_execution_context.WithIoWriteRcCost(0),
		}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := ioCtx(t, "vscprice", tc.opts...)

			io := ctx.IOSession()
			if res := ctx.SetState(key, value); res.IsErr() {
				t.Fatalf("SetState: %v", res.UnwrapErr())
			}
			got := int(io.End())

			// The key is read (1 RC a byte); the value is all new.
			want := (len(key)*params.READ_IO_GAS_RC_COST + len(value)*tc.want) * params.CYCLE_GAS_PER_RC
			if got != want {
				t.Fatalf("storing %d new bytes cost %d gas, want %d (%d RC a new byte)", len(value), got, want, tc.want)
			}
			if total := ctx.IOGas(); total != want {
				t.Fatalf("IOGas() = %d, want %d: the running total and the host call's share use one formula", total, want)
			}
		})
	}
}

// Only NEW bytes follow the write price. Bytes read, rewritten in place and
// logged are charged the read price at either setting, so the 0.15.0 cut leaves
// them exactly where they were.
func TestReadsRewritesAndLogsDoNotFollowTheWritePrice(t *testing.T) {
	const key = "k"
	value := strings.Repeat("x", 1000)

	measure := func(price int) (rewrite, read, log int) {
		ctx := ioCtx(t, "vscprice", contract_execution_context.WithIoWriteRcCost(price))
		ctx.SetState(key, value) // first write, not measured

		io := ctx.IOSession()
		ctx.SetState(key, value)
		rewrite = int(io.End())

		io = ctx.IOSession()
		ctx.GetState(key)
		read = int(io.End())

		io = ctx.IOSession()
		ctx.Log(value)
		log = int(io.End())
		return
	}

	oldRewrite, oldRead, oldLog := measure(params.WRITE_IO_GAS_RC_COST)
	newRewrite, newRead, newLog := measure(params.WRITE_IO_GAS_RC_COST_V0_15)

	// A rewrite reads the key and the old value, then pays for the bytes it
	// puts back: about 2 RC a byte.
	wantRewrite := (len(key) + 2*len(value)) * params.CYCLE_GAS_PER_RC
	wantRead := (len(key) + len(value)) * params.CYCLE_GAS_PER_RC
	wantLog := len(value) * params.CYCLE_GAS_PER_RC
	for _, c := range []struct {
		name           string
		old, new, want int
	}{
		{"rewrite", oldRewrite, newRewrite, wantRewrite},
		{"read", oldRead, newRead, wantRead},
		{"log", oldLog, newLog, wantLog},
	} {
		if c.old != c.want || c.new != c.want {
			t.Errorf("%s of %d bytes: %d gas at the original price, %d at the 0.15.0 price, want %d at both",
				c.name, len(value), c.old, c.new, c.want)
		}
	}
}

// A value that grows pays the write price only for the bytes past the
// contract's largest size so far; one written below that mark (after a delete)
// pays the read price. This is the contract-size accounting the price sits on,
// pinned at both prices so a later change to it shows up here.
func TestWritePriceAppliesOnlyPastTheContractsLargestSize(t *testing.T) {
	for _, price := range []int{params.WRITE_IO_GAS_RC_COST, params.WRITE_IO_GAS_RC_COST_V0_15} {
		ctx := ioCtx(t, "vscprice", contract_execution_context.WithIoWriteRcCost(price))
		ctx.SetState("a", strings.Repeat("x", 1000)) // largest size so far: 1000

		// Grow by 500: the key and the 1000 old bytes are read, the old bytes
		// are charged once more as rewritten, and 500 are new.
		io := ctx.IOSession()
		ctx.SetState("a", strings.Repeat("x", 1500))
		if got, want := int(io.End()), (1+2*1000+500*price)*params.CYCLE_GAS_PER_RC; got != want {
			t.Errorf("price %d: growing a value by 500 cost %d gas, want %d", price, got, want)
		}

		// Delete it, then store 1200 bytes under a new key: the contract was
		// larger before, so none of it is new.
		io = ctx.IOSession()
		ctx.DeleteState("a")
		io.End()
		io = ctx.IOSession()
		ctx.SetState("b", strings.Repeat("y", 1200))
		if got, want := int(io.End()), (1+1200)*params.CYCLE_GAS_PER_RC; got != want {
			t.Errorf("price %d: a write below the largest size cost %d gas, want %d (read price only)", price, got, want)
		}
	}
}
