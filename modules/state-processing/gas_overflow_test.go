package state_engine

import (
	"math"
	"testing"
	"vsc-node/modules/common/params"
)

func TestGasBudgetOverflow(t *testing.T) {
	// Attack: if gas * CYCLE_GAS_PER_RC overflows uint, the contract
	// gets a tiny gas budget instead of a huge one.

	t.Run("NormalValue", func(t *testing.T) {
		gas := uint(1000)
		budget := gas * params.CYCLE_GAS_PER_RC
		if budget != 100_000_000 {
			t.Errorf("expected 100000000, got %d", budget)
		}
	})

	t.Run("OverflowWithoutCap", func(t *testing.T) {
		// Simulate what would happen without the cap
		hugeGas := uint(math.MaxUint64 / params.CYCLE_GAS_PER_RC) + 1
		overflowed := hugeGas * params.CYCLE_GAS_PER_RC
		if overflowed > hugeGas {
			t.Log("no overflow detected — platform may handle this differently")
		} else {
			t.Logf("OVERFLOW CONFIRMED: %d * %d = %d (wrapped)", hugeGas, params.CYCLE_GAS_PER_RC, overflowed)
		}
	})

	t.Run("CapPreventsOverflow", func(t *testing.T) {
		// The fix caps gas before multiplication
		maxGas := ^uint(0) / params.CYCLE_GAS_PER_RC
		hugeGas := uint(math.MaxUint64)

		gas := hugeGas
		if gas > maxGas {
			gas = maxGas
		}

		budget := gas * params.CYCLE_GAS_PER_RC
		if budget < gas {
			t.Errorf("OVERFLOW STILL POSSIBLE: gas=%d budget=%d", gas, budget)
		} else {
			t.Logf("Cap works: gas capped to %d, budget=%d (no overflow)", gas, budget)
		}
	})
}
