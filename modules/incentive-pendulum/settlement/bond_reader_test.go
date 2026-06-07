package settlement

import (
	"errors"
	"testing"

	ledgerDb "vsc-node/modules/db/vsc/ledger"
)

type stubBalanceReader struct {
	records map[string]*ledgerDb.BalanceRecord
}

func (s *stubBalanceReader) GetBalanceRecord(account string, blockHeight uint64) (*ledgerDb.BalanceRecord, error) {
	rec, ok := s.records[account]
	if !ok {
		return nil, nil
	}
	return rec, nil
}

// erringBalanceReader returns an error for samples at/after errAtOrAbove,
// simulating a per-node transient read failure mid-window.
type erringBalanceReader struct {
	records      map[string]*ledgerDb.BalanceRecord
	errAtOrAbove uint64
}

func (s *erringBalanceReader) GetBalanceRecord(account string, blockHeight uint64) (*ledgerDb.BalanceRecord, error) {
	if blockHeight >= s.errAtOrAbove {
		return nil, errors.New("simulated transient ledger read error")
	}
	rec, ok := s.records[account]
	if !ok {
		return nil, nil
	}
	return rec, nil
}

// TestReadCommitteeBonds_FailStopsOnReadError proves the GAP-1 fix: a per-node
// transient read error must make ReadCommitteeBonds return (nil, err) — NOT a
// partial bond map that silently dropped the errored (possibly minimum) sample,
// which would diverge the settlement CID across nodes. The election bond gate
// fail-stops on the same error class; settlement now matches.
func TestReadCommitteeBonds_FailStopsOnReadError(t *testing.T) {
	reader := &erringBalanceReader{
		records: map[string]*ledgerDb.BalanceRecord{
			"hive:alice": {Account: "hive:alice", HIVE_CONSENSUS: 1_000_000},
		},
		errAtOrAbove: 95, // window (90,100] samples will include >=95 → error
	}
	bonds, err := ReadCommitteeBonds(reader, []string{"hive:alice"}, 90, 100)
	if err == nil {
		t.Fatal("expected a non-nil error when a sample read fails (fail-stop), got nil")
	}
	if bonds != nil {
		t.Fatalf("expected nil bonds on read error (no partial/divergent map), got %+v", bonds)
	}
}

func TestReadCommitteeBonds_ReadsHIVEConsensusDirectly(t *testing.T) {
	// The whole point of this helper: BalanceRecord.HIVE_CONSENSUS bypasses
	// LedgerSystem.GetBalance's op-type filter that misses freshly-staked
	// consensus_stake ops.
	reader := &stubBalanceReader{records: map[string]*ledgerDb.BalanceRecord{
		"hive:alice": {Account: "hive:alice", HIVE_CONSENSUS: 1_000_000},
		"hive:bob":   {Account: "hive:bob", HIVE_CONSENSUS: 500_000},
	}}
	bonds, _ := ReadCommitteeBonds(reader, []string{"hive:alice", "hive:bob"}, 90, 100)
	if bonds["hive:alice"] != 1_000_000 {
		t.Errorf("alice: got %d want 1000000", bonds["hive:alice"])
	}
	if bonds["hive:bob"] != 500_000 {
		t.Errorf("bob: got %d want 500000", bonds["hive:bob"])
	}
}

func TestReadCommitteeBonds_NormalizesAccountPrefix(t *testing.T) {
	reader := &stubBalanceReader{records: map[string]*ledgerDb.BalanceRecord{
		"hive:alice": {Account: "hive:alice", HIVE_CONSENSUS: 999},
	}}
	// Caller passes plain "alice"; helper prepends "hive:" before the lookup.
	bonds, _ := ReadCommitteeBonds(reader, []string{"alice"}, 90, 100)
	if bonds["hive:alice"] != 999 {
		t.Fatalf("expected normalized lookup, got %+v", bonds)
	}
}

func TestReadCommitteeBonds_OmitsZeroBonds(t *testing.T) {
	reader := &stubBalanceReader{records: map[string]*ledgerDb.BalanceRecord{
		"hive:alice": {Account: "hive:alice", HIVE_CONSENSUS: 1000},
		"hive:bob":   {Account: "hive:bob", HIVE_CONSENSUS: 0},
		"hive:carol": {Account: "hive:carol", HIVE_CONSENSUS: -5}, // defensive
	}}
	bonds, _ := ReadCommitteeBonds(reader, []string{"hive:alice", "hive:bob", "hive:carol"}, 90, 100)
	if _, ok := bonds["hive:bob"]; ok {
		t.Error("bob with 0 bond should be omitted")
	}
	if _, ok := bonds["hive:carol"]; ok {
		t.Error("carol with negative bond should be omitted")
	}
	if len(bonds) != 1 {
		t.Errorf("expected 1 bond entry, got %d: %+v", len(bonds), bonds)
	}
}

func TestReadCommitteeBonds_NilReaderSafe(t *testing.T) {
	bonds, _ := ReadCommitteeBonds(nil, []string{"hive:alice"}, 90, 100)
	if bonds != nil {
		t.Fatalf("nil reader should yield nil, got %+v", bonds)
	}
}

func TestReadCommitteeBonds_EmptyMembersSafe(t *testing.T) {
	reader := &stubBalanceReader{records: map[string]*ledgerDb.BalanceRecord{}}
	bonds, _ := ReadCommitteeBonds(reader, nil, 90, 100)
	if bonds != nil {
		t.Fatalf("empty members should yield nil, got %+v", bonds)
	}
}

func TestReadCommitteeBonds_MissingAccountSkipped(t *testing.T) {
	// One member never deposited / staked → no balance record. Helper must
	// silently skip rather than returning a partial map with a 0 entry,
	// which would skew distribution math downstream.
	reader := &stubBalanceReader{records: map[string]*ledgerDb.BalanceRecord{
		"hive:alice": {Account: "hive:alice", HIVE_CONSENSUS: 1000},
	}}
	bonds, _ := ReadCommitteeBonds(reader, []string{"hive:alice", "hive:bob"}, 90, 100)
	if _, ok := bonds["hive:bob"]; ok {
		t.Error("bob with missing record should be omitted")
	}
	if bonds["hive:alice"] != 1000 {
		t.Errorf("alice should still be present, got %d", bonds["hive:alice"])
	}
}
