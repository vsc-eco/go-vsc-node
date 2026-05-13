package state_engine_test

import (
	"math/big"
	"testing"

	pendulumoracle "vsc-node/modules/incentive-pendulum/oracle"
	state_engine "vsc-node/modules/state-processing"
)

// stubStates is an in-memory PoolStateKeyReader for tests. Keyed by
// (contractID, key) — block-height pinning isn't exercised here because
// the production reader's height handling is a thin pass-through to
// ContractState.GetLastOutput, which has its own coverage.
type stubStates struct {
	values map[string]map[string][]byte
}

func newStubStates() *stubStates {
	return &stubStates{values: make(map[string]map[string][]byte)}
}

func (s *stubStates) ReadStateKey(contractID string, blockHeight uint64, key string) ([]byte, bool) {
	c, ok := s.values[contractID]
	if !ok {
		return nil, false
	}
	v, ok := c[key]
	return v, ok
}

func (s *stubStates) setStr(contractID, key, val string) {
	if _, ok := s.values[contractID]; !ok {
		s.values[contractID] = make(map[string][]byte)
	}
	s.values[contractID][key] = []byte(val)
}

// setBigInt mirrors the dex contract's setBigInt — store big.Int.Bytes()
// (unsigned big-endian) as raw bytes.
func (s *stubStates) setBigInt(contractID, key string, val int64) {
	if _, ok := s.values[contractID]; !ok {
		s.values[contractID] = make(map[string][]byte)
	}
	s.values[contractID][key] = big.NewInt(val).Bytes()
}

func readerForTest(states pendulumoracle.PoolReserveReader) pendulumoracle.PoolReserveReader {
	return states
}

func newPoolReserveReader(t *testing.T, states *stubStates) pendulumoracle.PoolReserveReader {
	t.Helper()
	se := state_engine.NewForGeometryTest(nil, nil, nil, nil)
	return se.PendulumPoolReserveReaderForTest(states)
}

// TestPoolReserveReader_ReadsHBDOnAsset0 — a0n="hbd" → reads r0.
func TestPoolReserveReader_ReadsHBDOnAsset0(t *testing.T) {
	states := newStubStates()
	states.setStr("vsc1pool", "a0n", "hbd")
	states.setStr("vsc1pool", "a1n", "hive")
	states.setBigInt("vsc1pool", "r0", 1_500_000)
	states.setBigInt("vsc1pool", "r1", 4_200_000)

	reader := newPoolReserveReader(t, states)
	got, ok := reader.ReadPoolHBDReserve("vsc1pool", 200)
	if !ok {
		t.Fatal("expected ok=true for HBD-paired pool")
	}
	if got != 1_500_000 {
		t.Errorf("got %d, want 1500000 (asset0 reserve r0)", got)
	}
}

// TestPoolReserveReader_ReadsHBDOnAsset1 — a1n="hbd" → reads r1.
func TestPoolReserveReader_ReadsHBDOnAsset1(t *testing.T) {
	states := newStubStates()
	states.setStr("vsc1pool", "a0n", "hive")
	states.setStr("vsc1pool", "a1n", "hbd")
	states.setBigInt("vsc1pool", "r0", 4_200_000)
	states.setBigInt("vsc1pool", "r1", 1_500_000)

	reader := newPoolReserveReader(t, states)
	got, ok := reader.ReadPoolHBDReserve("vsc1pool", 200)
	if !ok {
		t.Fatal("expected ok=true for HBD-paired pool")
	}
	if got != 1_500_000 {
		t.Errorf("got %d, want 1500000 (asset1 reserve r1)", got)
	}
}

// TestPoolReserveReader_NameIsCaseInsensitive — guards against the contract
// writing "HBD" instead of "hbd". The lookup lowercases before comparing.
func TestPoolReserveReader_NameIsCaseInsensitive(t *testing.T) {
	states := newStubStates()
	states.setStr("vsc1pool", "a0n", "HBD")
	states.setStr("vsc1pool", "a1n", "HIVE")
	states.setBigInt("vsc1pool", "r0", 999)
	states.setBigInt("vsc1pool", "r1", 1)

	reader := newPoolReserveReader(t, states)
	got, ok := reader.ReadPoolHBDReserve("vsc1pool", 200)
	if !ok || got != 999 {
		t.Fatalf("got (%d,%v), want (999,true)", got, ok)
	}
}

// TestPoolReserveReader_NonHBDPairRejected — a pool with no HBD side never
// contributes a P_hbd term. This is what shields V from a pool that's
// whitelisted by mistake or via a governance race.
func TestPoolReserveReader_NonHBDPairRejected(t *testing.T) {
	states := newStubStates()
	states.setStr("vsc1pool", "a0n", "hive")
	states.setStr("vsc1pool", "a1n", "btc")
	states.setBigInt("vsc1pool", "r0", 1_000_000)
	states.setBigInt("vsc1pool", "r1", 1_000_000)

	reader := newPoolReserveReader(t, states)
	if amt, ok := reader.ReadPoolHBDReserve("vsc1pool", 200); ok || amt != 0 {
		t.Fatalf("expected (0,false) for non-HBD pair, got (%d,%v)", amt, ok)
	}
}

// TestPoolReserveReader_ZeroReserveReturnsFalse — a pool with the names
// populated but zero reserve (e.g., post-deploy, pre-liquidity) doesn't
// count toward V.
func TestPoolReserveReader_ZeroReserveReturnsFalse(t *testing.T) {
	states := newStubStates()
	states.setStr("vsc1pool", "a0n", "hbd")
	states.setStr("vsc1pool", "a1n", "hive")
	// r0/r1 absent — getBigInt would return zero-bytes; here we test the
	// "missing key" path.

	reader := newPoolReserveReader(t, states)
	if amt, ok := reader.ReadPoolHBDReserve("vsc1pool", 200); ok || amt != 0 {
		t.Fatalf("expected (0,false) for zero-reserve pool, got (%d,%v)", amt, ok)
	}
}

// TestPoolReserveReader_LedgerSurplusIgnored is the load-bearing
// regression for issue #8: an attacker dumping unsolicited HBD into the
// pool's contract ledger account does not change the reported reserve,
// because the reader pulls from the contract's self-declared r0/r1, not
// from the ledger balance.
//
// (The stub doesn't model the ledger at all; the test asserts that the
// reader's return value is the contract's r0 — establishing that any
// hypothetical ledger surplus is structurally invisible to V.)
func TestPoolReserveReader_LedgerSurplusIgnored(t *testing.T) {
	states := newStubStates()
	states.setStr("vsc1pool", "a0n", "hbd")
	states.setStr("vsc1pool", "a1n", "hive")
	states.setBigInt("vsc1pool", "r0", 1_000_000)
	states.setBigInt("vsc1pool", "r1", 1_000_000)

	// If the reader were still reading ledger balance, an attacker doubling
	// the contract account's HBD via a transfer would surface here. With
	// the new path the reader only sees r0 — the contract's own bookkeeping.
	reader := newPoolReserveReader(t, states)
	got, ok := reader.ReadPoolHBDReserve("vsc1pool", 200)
	if !ok || got != 1_000_000 {
		t.Fatalf("got (%d,%v), want (1000000,true) — ledger surplus must not bleed in", got, ok)
	}
}

// TestPoolReserveReader_UnknownContractReturnsFalse — never-deployed pool.
func TestPoolReserveReader_UnknownContractReturnsFalse(t *testing.T) {
	states := newStubStates()

	reader := newPoolReserveReader(t, states)
	if amt, ok := reader.ReadPoolHBDReserve("vsc1unknown", 200); ok || amt != 0 {
		t.Fatalf("expected (0,false) for unknown contract, got (%d,%v)", amt, ok)
	}
}

// TestPoolReserveReader_EmptyContractIDRejected — defence in depth.
func TestPoolReserveReader_EmptyContractIDRejected(t *testing.T) {
	states := newStubStates()
	reader := newPoolReserveReader(t, states)
	if amt, ok := reader.ReadPoolHBDReserve("", 200); ok || amt != 0 {
		t.Fatalf("expected (0,false) for empty contract id, got (%d,%v)", amt, ok)
	}
	if amt, ok := reader.ReadPoolHBDReserve("   ", 200); ok || amt != 0 {
		t.Fatalf("expected (0,false) for whitespace contract id, got (%d,%v)", amt, ok)
	}
}

// keep readerForTest referenced even if a future refactor inlines newPoolReserveReader.
var _ = readerForTest
