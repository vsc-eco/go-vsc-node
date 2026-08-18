package state_engine_test

import (
	"errors"
	"testing"

	ledgerDb "vsc-node/modules/db/vsc/ledger"
	stateEngine "vsc-node/modules/state-processing"

	"github.com/stretchr/testify/assert"
)

// The monitor replaces the removed fail-stop guard. It must never influence
// block processing — read-only, no panic, no error return — because the guard
// it replaces became a halt vector precisely by acting on what it found.
func TestNegativeBalanceScan_IsReadOnlyAndNeverPanics(t *testing.T) {
	env := newTestEnv()
	env.BalanceDb.BalanceRecords["hive:dhedge"] = []ledgerDb.BalanceRecord{{
		Account: "hive:dhedge", BlockHeight: stateEngine.NegativeBalanceScanInterval,
		HBD_SAVINGS: -283,
	}}
	before := len(env.BalanceDb.BalanceRecords["hive:dhedge"])

	assert.NotPanics(t, func() {
		env.SE.ScanNegativeBalancesForTest(stateEngine.NegativeBalanceScanInterval)
	}, "the monitor must never panic — the guard it replaces halted mainnet by acting on a finding")

	assert.Equal(t, before, len(env.BalanceDb.BalanceRecords["hive:dhedge"]),
		"the monitor must not write anything")
	assert.Equal(t, int64(-283), env.BalanceDb.BalanceRecords["hive:dhedge"][0].HBD_SAVINGS,
		"and must not alter the balance it reports")
}

// A read failure must be swallowed, not fail-stopped: this is observability,
// and wedging block processing on it would reintroduce a halt vector.
func TestNegativeBalanceScan_ReadFailureDoesNotWedgeTheSlot(t *testing.T) {
	env := newTestEnv()
	env.BalanceDb.GetAllErr = errors.New("connection reset by peer")

	assert.NotPanics(t, func() {
		env.SE.ScanNegativeBalancesForTest(stateEngine.NegativeBalanceScanInterval)
	}, "a failed read must not block the slot")
}

// It runs on an interval, not every slot.
func TestNegativeBalanceScan_RunsOnlyOnItsInterval(t *testing.T) {
	env := newTestEnv()
	env.BalanceDb.GetAllErr = errors.New("would be hit if it scanned")
	assert.NotPanics(t, func() {
		env.SE.ScanNegativeBalancesForTest(stateEngine.NegativeBalanceScanInterval + 1)
	}, "an off-interval height must not scan at all")
}
