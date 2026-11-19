package params

import (
	"testing"

	"vsc-node/modules/common/consensusversion"
)

// The storage write price is chosen by the chain-active consensus version and
// nothing else: 19 RC a new byte up to 0.14.0, 1 RC from 0.15.0. The price
// decides whether a call finishes inside its rc_limit, so a node that picked it
// any other way (its own binary version, a config value) would disagree with
// the rest about a call's result.
func TestWriteIoGasRcCostFollowsTheActiveVersion(t *testing.T) {
	v := func(major, consensus uint64) consensusversion.Version {
		return consensusversion.Version{Major: major, Consensus: consensus}
	}
	cases := []struct {
		name   string
		active consensusversion.Version
		want   int
	}{
		{"no election stored yet", consensusversion.Version{}, 19},
		{"0.12.0", v(0, 12), 19},
		{"0.14.0, mainnet when this shipped", v(0, 14), 19},
		{"0.15.0, the line itself", v(0, 15), 1},
		{"0.15.3, a non-consensus bump on the line", consensusversion.Version{Consensus: 15, NonConsensus: 3}, 1},
		{"0.16.0, a later line carries it", v(0, 16), 1},
		// Every gate compares the components independently, so a major bump
		// alone does not carry a line (see vault_gates_test.go): a release that
		// raised the major and reset the consensus component would turn every
		// gate off, and this one would then charge 19 again. A major bump has
		// to carry the consensus component forward (1.15.0, not 1.0.0).
		{"1.0.0", v(1, 0), 19},
		{"1.15.0", v(1, 15), 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := WriteIoGasRcCost(tc.active); got != tc.want {
				t.Fatalf("WriteIoGasRcCost(%s) = %d, want %d", tc.active.Format(), got, tc.want)
			}
			if got, want := consensusversion.WriteIoPriceCutActive(tc.active), tc.want == WRITE_IO_GAS_RC_COST_V0_15; got != want {
				t.Fatalf("WriteIoPriceCutActive(%s) = %v, want %v", tc.active.Format(), got, want)
			}
		})
	}
}

// The schedule never returns a price below 1 for any version: a new stored
// byte is never free. (The execution context refuses a price below 1 and keeps
// the original one; with this pinned that refusal is never reached.)
func TestWriteIoGasRcCostIsNeverBelowOne(t *testing.T) {
	for major := uint64(0); major <= 3; major++ {
		for consensus := uint64(0); consensus <= 64; consensus++ {
			active := consensusversion.Version{Major: major, Consensus: consensus}
			if got := WriteIoGasRcCost(active); got < 1 {
				t.Fatalf("WriteIoGasRcCost(%s) = %d, want at least 1", active.Format(), got)
			}
		}
	}
}

// The two prices themselves. A change to either is a consensus change: the
// first re-prices settled history, the second re-prices every call on 0.15.0.
func TestWriteIoPricesArePinned(t *testing.T) {
	if WRITE_IO_GAS_RC_COST != 19 {
		t.Fatalf("WRITE_IO_GAS_RC_COST = %d, want 19: history up to 0.15.0 was charged 19", WRITE_IO_GAS_RC_COST)
	}
	if WRITE_IO_GAS_RC_COST_V0_15 != 1 {
		t.Fatalf("WRITE_IO_GAS_RC_COST_V0_15 = %d, want 1", WRITE_IO_GAS_RC_COST_V0_15)
	}
	if READ_IO_GAS_RC_COST != 1 {
		t.Fatalf("READ_IO_GAS_RC_COST = %d, want 1", READ_IO_GAS_RC_COST)
	}
}
