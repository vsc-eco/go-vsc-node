package state_engine_test

import (
	"testing"

	"vsc-node/lib/test_utils"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	pendulum_oracle "vsc-node/modules/db/vsc/pendulum_oracle"
	pendulumoracle "vsc-node/modules/incentive-pendulum/oracle"
	pendulumsettlement "vsc-node/modules/incentive-pendulum/settlement"
	ledgerSystem "vsc-node/modules/ledger-system"
	state_engine "vsc-node/modules/state-processing"
)

// poolReserveReaderEnv standsBuilds the minimum surface the production
// pool-reserve reader touches: a LedgerSystem backed by mock balance/ledger
// DBs. The W7-rev2 read path is just LedgerSystem.GetBalance("contract:X",
// bh, "hbd"); no contract state, no datalayer required.
type poolReserveReaderEnv struct {
	se    *state_engine.StateEngine
	balDb *test_utils.MockBalanceDb
}

func newPoolReserveReaderEnv(t *testing.T) poolReserveReaderEnv {
	t.Helper()

	balDb := &test_utils.MockBalanceDb{
		BalanceRecords: make(map[string][]ledgerDb.BalanceRecord),
	}
	lDb := &test_utils.MockLedgerDb{
		LedgerRecords: make(map[string][]ledgerDb.LedgerRecord),
	}
	aDb := &test_utils.MockActionsDb{
		Actions: make(map[string]ledgerDb.ActionRecord),
	}
	ls := ledgerSystem.New(balDb, lDb, nil, aDb)
	se := state_engine.NewForPendulumSettlementTest(ls, nil, balDb, nil, (pendulumsettlement.Broadcaster)(nil))
	return poolReserveReaderEnv{se: se, balDb: balDb}
}

// readerForTest grabs the production reader via the geometry computer the
// test_constructor wires up. Going through the GeometryComputer surface
// keeps the test honest about the actual production code path.
func readerForTest(env poolReserveReaderEnv) pendulumoracle.PoolReserveReader {
	return env.se.PendulumPoolReserveReaderForTest()
}

func seedContractHBDBalance(balDb *test_utils.MockBalanceDb, contractID string, bh uint64, hbd int64) {
	acct := "contract:" + contractID
	balDb.BalanceRecords[acct] = []ledgerDb.BalanceRecord{{
		Account:     acct,
		BlockHeight: bh,
		HBD:         hbd,
	}}
}

func TestPoolReserveReader_ReadsContractHBDBalance(t *testing.T) {
	env := newPoolReserveReaderEnv(t)
	seedContractHBDBalance(env.balDb, "vsc1pool", 100, 1_500_000)

	reader := readerForTest(env)
	got, ok := reader.ReadPoolHBDReserve("vsc1pool", 200)
	if !ok {
		t.Fatal("expected ok=true for funded pool")
	}
	if got != 1_500_000 {
		t.Errorf("got %d want 1500000", got)
	}
}

func TestPoolReserveReader_ZeroBalanceReturnsFalse(t *testing.T) {
	env := newPoolReserveReaderEnv(t)
	// Pool exists in the balance DB but holds no HBD (e.g. fresh deploy).
	seedContractHBDBalance(env.balDb, "vsc1empty", 100, 0)

	reader := readerForTest(env)
	if amt, ok := reader.ReadPoolHBDReserve("vsc1empty", 200); ok || amt != 0 {
		t.Fatalf("expected (0,false) for zero-balance pool, got (%d,%v)", amt, ok)
	}
}

func TestPoolReserveReader_UnknownContractReturnsFalse(t *testing.T) {
	env := newPoolReserveReaderEnv(t)
	// No record seeded.

	reader := readerForTest(env)
	if amt, ok := reader.ReadPoolHBDReserve("vsc1unknown", 200); ok || amt != 0 {
		t.Fatalf("expected (0,false) for unknown contract, got (%d,%v)", amt, ok)
	}
}

func TestPoolReserveReader_EmptyContractIDRejected(t *testing.T) {
	env := newPoolReserveReaderEnv(t)
	reader := readerForTest(env)
	if amt, ok := reader.ReadPoolHBDReserve("", 200); ok || amt != 0 {
		t.Fatalf("expected (0,false) for empty contract id, got (%d,%v)", amt, ok)
	}
	if amt, ok := reader.ReadPoolHBDReserve("   ", 200); ok || amt != 0 {
		t.Fatalf("expected (0,false) for whitespace contract id, got (%d,%v)", amt, ok)
	}
}

// silence unused warnings if the snapshot db field becomes optional later.
var _ pendulum_oracle.PendulumOracleSnapshots = (pendulum_oracle.PendulumOracleSnapshots)(nil)
