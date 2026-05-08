package state_engine

import (
	"strings"
	"testing"

	"vsc-node/modules/db/vsc/elections"
	pendulumsettlement "vsc-node/modules/incentive-pendulum/settlement"

	"github.com/chebyrash/promise"
)

// stubElectionsByHeight is a minimal elections.Elections impl satisfying
// the validator's read needs without dragging in test_utils (which would
// cause an import cycle for an internal test in this package).
type stubElectionsByHeight struct {
	atHeight map[uint64]elections.ElectionResult
}

func (s *stubElectionsByHeight) Init() error                  { return nil }
func (s *stubElectionsByHeight) Start() *promise.Promise[any] { return nil }
func (s *stubElectionsByHeight) Stop() error                  { return nil }
func (s *stubElectionsByHeight) StoreElection(elections.ElectionResult) error {
	return nil
}
func (s *stubElectionsByHeight) GetElection(uint64) *elections.ElectionResult { return nil }
func (s *stubElectionsByHeight) GetPreviousElections(uint64, int) []elections.ElectionResult {
	return nil
}
func (s *stubElectionsByHeight) GetElectionByHeight(h uint64) (elections.ElectionResult, error) {
	if r, ok := s.atHeight[h]; ok {
		return r, nil
	}
	return elections.ElectionResult{}, nil
}

// withElection installs a minimal election at snapshotHeight whose members
// match the provided account list (without "hive:" prefix; the validator
// normalises both sides).
func withElection(snapshotHeight uint64, accounts ...string) *StateEngine {
	mems := make([]elections.ElectionMember, 0, len(accounts))
	for _, a := range accounts {
		mems = append(mems, elections.ElectionMember{Account: a, Key: a + "-key"})
	}
	return &StateEngine{
		electionDb: &stubElectionsByHeight{atHeight: map[uint64]elections.ElectionResult{
			snapshotHeight: {
				ElectionDataInfo: elections.ElectionDataInfo{Members: mems},
			},
		}},
	}
}

func validRec(snapshotTo uint64, distributions []pendulumsettlement.DistributionEntry) pendulumsettlement.SettlementRecord {
	var sum int64
	for _, d := range distributions {
		sum += d.HBDAmt
	}
	return pendulumsettlement.SettlementRecord{
		Epoch:               5,
		PrevEpoch:           4,
		SnapshotRangeFrom:   1000,
		SnapshotRangeTo:     snapshotTo,
		BucketBalanceHBD:    sum,
		TotalDistributedHBD: sum,
		ResidualHBD:         0,
		Distributions:       distributions,
	}
}

func TestValidate_AcceptsHonestRecord(t *testing.T) {
	se := withElection(2000, "alice", "bob")
	rec := validRec(2000, []pendulumsettlement.DistributionEntry{
		{Account: "hive:alice", HBDAmt: 600},
		{Account: "hive:bob", HBDAmt: 400},
	})
	if err := se.validatePendulumSettlement(rec); err != nil {
		t.Fatalf("honest record rejected: %v", err)
	}
}

func TestValidate_RejectsTotalExceedsBucket(t *testing.T) {
	// User-listed minimum assertion: TotalDistributedHBD ≤ BucketBalanceHBD.
	se := withElection(2000, "alice")
	rec := validRec(2000, []pendulumsettlement.DistributionEntry{
		{Account: "hive:alice", HBDAmt: 1000},
	})
	rec.BucketBalanceHBD = 500 // smaller than TotalDistributedHBD
	rec.ResidualHBD = -500     // would-be conservation; doesn't matter, this guard fires first

	if err := se.validatePendulumSettlement(rec); err == nil ||
		!strings.Contains(err.Error(), "exceeds BucketBalanceHBD") {
		t.Fatalf("expected over-spend rejection, got err=%v", err)
	}
}

func TestValidate_RejectsNegativeFields(t *testing.T) {
	se := withElection(2000, "alice")
	cases := []struct {
		name string
		mut  func(*pendulumsettlement.SettlementRecord)
	}{
		{"negative bucket", func(r *pendulumsettlement.SettlementRecord) { r.BucketBalanceHBD = -1 }},
		{"negative total distributed", func(r *pendulumsettlement.SettlementRecord) { r.TotalDistributedHBD = -1 }},
		{"negative residual", func(r *pendulumsettlement.SettlementRecord) { r.ResidualHBD = -1 }},
	}
	for _, tc := range cases {
		rec := validRec(2000, []pendulumsettlement.DistributionEntry{{Account: "hive:alice", HBDAmt: 100}})
		tc.mut(&rec)
		if err := se.validatePendulumSettlement(rec); err == nil {
			t.Errorf("%s: expected rejection", tc.name)
		}
	}
}

func TestValidate_RejectsConservationBreak(t *testing.T) {
	se := withElection(2000, "alice")
	rec := validRec(2000, []pendulumsettlement.DistributionEntry{
		{Account: "hive:alice", HBDAmt: 500},
	})
	rec.BucketBalanceHBD = 1000 // distributed=500, residual=0, but bucket=1000 → conservation breaks
	if err := se.validatePendulumSettlement(rec); err == nil ||
		!strings.Contains(err.Error(), "conservation") {
		t.Fatalf("expected conservation-broken rejection, got err=%v", err)
	}
}

func TestValidate_RejectsSumMismatch(t *testing.T) {
	se := withElection(2000, "alice", "bob")
	rec := validRec(2000, []pendulumsettlement.DistributionEntry{
		{Account: "hive:alice", HBDAmt: 600},
		{Account: "hive:bob", HBDAmt: 400},
	})
	rec.TotalDistributedHBD = 900 // sum(distributions) is 1000
	rec.BucketBalanceHBD = 900
	rec.ResidualHBD = 0
	if err := se.validatePendulumSettlement(rec); err == nil ||
		!strings.Contains(err.Error(), "sum(distributions)") {
		t.Fatalf("expected sum-mismatch rejection, got err=%v", err)
	}
}

func TestValidate_RejectsNonMemberDistribution(t *testing.T) {
	// User-listed minimum assertion: every Distributions[].Account ∈ Election.Members.
	se := withElection(2000, "alice", "bob")
	rec := validRec(2000, []pendulumsettlement.DistributionEntry{
		{Account: "hive:alice", HBDAmt: 500},
		{Account: "hive:eve", HBDAmt: 500}, // not in committee
	})
	if err := se.validatePendulumSettlement(rec); err == nil ||
		!strings.Contains(err.Error(), "non-committee account hive:eve") {
		t.Fatalf("expected outsider-payment rejection, got err=%v", err)
	}
}

func TestValidate_RejectsNonMemberReduction(t *testing.T) {
	se := withElection(2000, "alice", "bob")
	rec := validRec(2000, []pendulumsettlement.DistributionEntry{
		{Account: "hive:alice", HBDAmt: 1000},
	})
	rec.RewardReductions = []pendulumsettlement.RewardReductionEntry{
		{Account: "hive:eve", Bps: 100},
	}
	if err := se.validatePendulumSettlement(rec); err == nil ||
		!strings.Contains(err.Error(), "non-committee account hive:eve") {
		t.Fatalf("expected non-member-reduction rejection, got err=%v", err)
	}
}

func TestValidate_NormalizesUnprefixedAccounts(t *testing.T) {
	// Compose may emit accounts without the "hive:" prefix; the validator
	// must tolerate that since the on-chain election has the same nuance.
	se := withElection(2000, "alice")
	rec := validRec(2000, []pendulumsettlement.DistributionEntry{
		{Account: "alice", HBDAmt: 500}, // unprefixed
	})
	if err := se.validatePendulumSettlement(rec); err != nil {
		t.Fatalf("unprefixed-but-valid account rejected: %v", err)
	}
}
