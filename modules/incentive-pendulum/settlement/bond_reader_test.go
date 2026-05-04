package settlement

import (
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

func TestReadCommitteeBonds_ReadsHIVEConsensusDirectly(t *testing.T) {
	// The whole point of this helper: BalanceRecord.HIVE_CONSENSUS bypasses
	// LedgerSystem.GetBalance's op-type filter that misses freshly-staked
	// consensus_stake ops.
	reader := &stubBalanceReader{records: map[string]*ledgerDb.BalanceRecord{
		"hive:alice": {Account: "hive:alice", HIVE_CONSENSUS: 1_000_000},
		"hive:bob":   {Account: "hive:bob", HIVE_CONSENSUS: 500_000},
	}}
	bonds := ReadCommitteeBonds(reader, []string{"hive:alice", "hive:bob"}, 100)
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
	bonds := ReadCommitteeBonds(reader, []string{"alice"}, 100)
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
	bonds := ReadCommitteeBonds(reader, []string{"hive:alice", "hive:bob", "hive:carol"}, 100)
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
	bonds := ReadCommitteeBonds(nil, []string{"hive:alice"}, 100)
	if bonds != nil {
		t.Fatalf("nil reader should yield nil, got %+v", bonds)
	}
}

func TestReadCommitteeBonds_EmptyMembersSafe(t *testing.T) {
	reader := &stubBalanceReader{records: map[string]*ledgerDb.BalanceRecord{}}
	bonds := ReadCommitteeBonds(reader, nil, 100)
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
	bonds := ReadCommitteeBonds(reader, []string{"hive:alice", "hive:bob"}, 100)
	if _, ok := bonds["hive:bob"]; ok {
		t.Error("bob with missing record should be omitted")
	}
	if bonds["hive:alice"] != 1000 {
		t.Errorf("alice should still be present, got %d", bonds["hive:alice"])
	}
}
