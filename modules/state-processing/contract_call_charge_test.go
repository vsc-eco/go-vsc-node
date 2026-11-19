package state_engine

import (
	"testing"

	"vsc-node/modules/common/consensusversion"
	"vsc-node/modules/common/params"
)

// What a contract call is charged. Up to consensus 0.14.0: the gas it used in
// whole RC, at least 100, even when one storage operation took it far past the
// RC it was given to spend. From 0.15.0: never more than that budget.
func TestContractCallRcCharge(t *testing.T) {
	v := func(c uint64) consensusversion.Version { return consensusversion.Version{Consensus: c} }
	gas := func(rc uint64) uint { return uint(rc * params.CYCLE_GAS_PER_RC) }
	cases := []struct {
		name    string
		gasUsed uint
		budget  uint64
		active  consensusversion.Version
		want    int64
	}{
		{"0.14: inside the budget", gas(4002), 10_000, v(14), 4002},
		{"0.14: gas rounds up to whole RC", gas(4002) + 1, 10_000, v(14), 4003},
		{"0.14: never under 100", gas(3), 10_000, v(14), 100},
		{"0.14: one operation past the budget is charged in full", gas(1_000_036), 1_000, v(14), 1_000_036},
		{"0.15: inside the budget, unchanged", gas(4002), 10_000, v(15), 4002},
		{"0.15: rounds up, unchanged", gas(4002) + 1, 10_000, v(15), 4003},
		{"0.15: exactly the budget", gas(10_000), 10_000, v(15), 10_000},
		{"0.15: past the budget is charged the budget", gas(1_000_036), 1_000, v(15), 1_000},
		{"0.15: one RC past the budget", gas(10_001), 10_000, v(15), 10_000},
		{"0.15: never under 100, also with a budget under 100", gas(900), 60, v(15), 100},
		{"0.15: never under 100", gas(3), 10_000, v(15), 100},
		{"0.16 carries it", gas(1_000_036), 1_000, v(16), 1_000},
		{"no election yet: the original charge", gas(1_000_036), 1_000, consensusversion.Version{}, 1_000_036},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ContractCallRcCharge(tc.gasUsed, tc.budget, tc.active); got != tc.want {
				t.Fatalf("ContractCallRcCharge(%d gas, budget %d, %s) = %d, want %d", tc.gasUsed, tc.budget, tc.active.Format(), got, tc.want)
			}
		})
	}
}
