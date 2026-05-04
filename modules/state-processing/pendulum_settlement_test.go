package state_engine_test

import (
	"testing"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/db/vsc/elections"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	pendulum_oracle "vsc-node/modules/db/vsc/pendulum_oracle"
	pendulumsettlement "vsc-node/modules/incentive-pendulum/settlement"
	ledgerSystem "vsc-node/modules/ledger-system"
	state_engine "vsc-node/modules/state-processing"

	"github.com/chebyrash/promise"
)

// stubSnapshotsDb implements pendulum_oracle.PendulumOracleSnapshots for the
// orchestration tests. Only GetSnapshotsInRange is exercised by the
// settlement path; the other methods are no-ops.
type stubSnapshotsDb struct {
	records []pendulum_oracle.SnapshotRecord
}

func (s *stubSnapshotsDb) Init() error { return nil }
func (s *stubSnapshotsDb) Start() *promise.Promise[any] {
	return promise.New(func(resolve func(any), reject func(error)) { resolve(nil) })
}
func (s *stubSnapshotsDb) Stop() error                                           { return nil }
func (s *stubSnapshotsDb) SaveSnapshot(rec pendulum_oracle.SnapshotRecord) error { return nil }
func (s *stubSnapshotsDb) GetSnapshot(_ uint64) (*pendulum_oracle.SnapshotRecord, bool, error) {
	return nil, false, nil
}
func (s *stubSnapshotsDb) GetSnapshotAtOrBefore(_ uint64) (*pendulum_oracle.SnapshotRecord, bool, error) {
	return nil, false, nil
}
func (s *stubSnapshotsDb) GetSnapshotsInRange(from, to uint64) ([]pendulum_oracle.SnapshotRecord, error) {
	out := make([]pendulum_oracle.SnapshotRecord, 0)
	for _, r := range s.records {
		if r.TickBlockHeight > from && r.TickBlockHeight <= to {
			out = append(out, r)
		}
	}
	return out, nil
}

// fakeBroadcaster captures the SettlementPayload passed to Broadcast so
// orchestration tests can assert on the wire-form output without standing
// up Hive.
type fakeBroadcaster struct {
	calls     []pendulumsettlement.SettlementPayload
	returnID  string
	returnErr error
}

func (f *fakeBroadcaster) Broadcast(payload pendulumsettlement.SettlementPayload) (string, error) {
	f.calls = append(f.calls, payload)
	if f.returnErr != nil {
		return "", f.returnErr
	}
	return f.returnID, nil
}

type orchEnv struct {
	se    *state_engine.StateEngine
	balDb *test_utils.MockBalanceDb
	elDb  *test_utils.MockElectionDb
	snaps *stubSnapshotsDb
	bcast *fakeBroadcaster
}

func newOrchestrationEnv(t *testing.T) orchEnv {
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
	elDb := &test_utils.MockElectionDb{
		Elections:         make(map[uint64]*elections.ElectionResult),
		ElectionsByHeight: make(map[uint64]elections.ElectionResult),
	}
	snaps := &stubSnapshotsDb{}
	bcast := &fakeBroadcaster{returnID: "fake-tx-id"}

	se := state_engine.NewForPendulumSettlementTest(ls, elDb, balDb, snaps, bcast)
	return orchEnv{se: se, balDb: balDb, elDb: elDb, snaps: snaps, bcast: bcast}
}

func seedElection(elDb *test_utils.MockElectionDb, epoch, blockHeight uint64, members ...string) {
	elMembers := make([]elections.ElectionMember, 0, len(members))
	for _, m := range members {
		elMembers = append(elMembers, elections.ElectionMember{Account: m})
	}
	res := elections.ElectionResult{
		ElectionCommonInfo: elections.ElectionCommonInfo{Epoch: epoch},
		ElectionDataInfo:   elections.ElectionDataInfo{Members: elMembers},
		BlockHeight:        blockHeight,
	}
	elDb.Elections[epoch] = &res
	elDb.ElectionsByHeight[blockHeight] = res
}

func seedBond(balDb *test_utils.MockBalanceDb, account string, blockHeight uint64, hiveConsensus int64) {
	balDb.BalanceRecords[account] = []ledgerDb.BalanceRecord{{
		Account:        account,
		BlockHeight:    blockHeight,
		HIVE_CONSENSUS: hiveConsensus,
	}}
}

func seedNodeBucket(ls ledgerSystem.LedgerSystem, amount int64, txID string, bh uint64) {
	ls.PendulumAccrue("nodes", "hbd", amount, txID, bh)
}

// TestOrchestration_EmptyBucketSkipsBroadcast pins Phase F decision #4
// from the plan: settlement broadcasts only when there are fees to clear.
func TestOrchestration_EmptyBucketSkipsBroadcast(t *testing.T) {
	env := newOrchestrationEnv(t)
	seedElection(env.elDb, 7, 1000, "alice", "bob")
	seedBond(env.balDb, "hive:alice", 999, 1_000_000)
	seedBond(env.balDb, "hive:bob", 999, 1_000_000)

	// nodes bucket left empty.
	if err := env.se.RunPendulumSettlementForTest(pendulumsettlement.BoundaryInfo{
		CurrentEpoch: 8, PreviousEpoch: 7, BlockHeight: 1100, Leader: "alice",
	}); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if len(env.bcast.calls) != 0 {
		t.Fatalf("expected zero broadcasts on empty bucket, got %d", len(env.bcast.calls))
	}
}

// TestOrchestration_LeaderEmitsExactPayload locks in the math: with two
// equal-bond witnesses and 1000 HBD in the bucket, each gets 500.
func TestOrchestration_LeaderEmitsExactPayload(t *testing.T) {
	env := newOrchestrationEnv(t)
	seedElection(env.elDb, 7, 1000, "alice", "bob")
	seedBond(env.balDb, "hive:alice", 999, 1_000_000)
	seedBond(env.balDb, "hive:bob", 999, 1_000_000)
	seedNodeBucket(env.se.LedgerSystem, 1000, "test-accrue", 999)

	if err := env.se.RunPendulumSettlementForTest(pendulumsettlement.BoundaryInfo{
		CurrentEpoch: 8, PreviousEpoch: 7, BlockHeight: 1100, Leader: "alice",
	}); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if len(env.bcast.calls) != 1 {
		t.Fatalf("expected exactly one broadcast, got %d", len(env.bcast.calls))
	}
	payload := env.bcast.calls[0]
	if payload.Epoch != 8 || payload.PrevEpoch != 7 {
		t.Errorf("payload epoch fields: got %+v", payload)
	}
	if len(payload.Dists) != 2 {
		t.Fatalf("expected 2 distributions, got %d", len(payload.Dists))
	}
	for _, d := range payload.Dists {
		if d.HBDAmt != 500 {
			t.Errorf("dist for %s: got %d want 500", d.Account, d.HBDAmt)
		}
	}
	if len(payload.Slashes) != 0 {
		t.Errorf("expected no slashes when no snapshots present, got %d", len(payload.Slashes))
	}
}

// TestOrchestration_DistributionResidualToLargestPostSlashStake pins the
// rule from the plan: floor-division residual goes to the largest
// post-slash stake, not the largest original stake.
func TestOrchestration_DistributionResidualToLargestPostSlashStake(t *testing.T) {
	env := newOrchestrationEnv(t)
	seedElection(env.elDb, 7, 1000, "alice", "bob")
	// alice originally larger; with 10% slash she falls below bob.
	seedBond(env.balDb, "hive:alice", 999, 1_000_000)
	seedBond(env.balDb, "hive:bob", 999, 950_000)
	seedNodeBucket(env.se.LedgerSystem, 100, "test-accrue", 999)

	// 10% (1000 bps, the per-epoch hard cap) slash on alice across the closed epoch.
	env.snaps.records = []pendulum_oracle.SnapshotRecord{{
		TickBlockHeight: 1050,
		WitnessSlashBps: []pendulum_oracle.WitnessSlashRecord{{Witness: "alice", Bps: 1000}},
	}}

	if err := env.se.RunPendulumSettlementForTest(pendulumsettlement.BoundaryInfo{
		CurrentEpoch: 8, PreviousEpoch: 7, BlockHeight: 1100, Leader: "alice",
	}); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if len(env.bcast.calls) != 1 {
		t.Fatalf("expected one broadcast, got %d", len(env.bcast.calls))
	}
	payload := env.bcast.calls[0]

	// alice post-slash = 900_000; bob post-slash = 950_000.
	// total = 1_850_000, R = 100.
	// alice = 100 * 900_000 / 1_850_000 = 48 (floor)
	// bob   = 100 * 950_000 / 1_850_000 = 51 (floor)
	// residual = 100 - (48+51) = 1 → bob (largest post-slash stake)
	wantAlice := int64(48)
	wantBob := int64(52) // 51 + 1 residual
	got := map[string]int64{}
	for _, d := range payload.Dists {
		got[d.Account] = d.HBDAmt
	}
	if got["hive:alice"] != wantAlice {
		t.Errorf("alice: got %d want %d", got["hive:alice"], wantAlice)
	}
	if got["hive:bob"] != wantBob {
		t.Errorf("bob (largest post-slash): got %d want %d", got["hive:bob"], wantBob)
	}
	if got["hive:alice"]+got["hive:bob"] != 100 {
		t.Errorf("distributions should sum to bucket balance: got %d want 100",
			got["hive:alice"]+got["hive:bob"])
	}
}

// TestOrchestration_AggregatesSlashesAcrossClosedEpoch verifies multi-tick
// slash aggregation lands in the slash payload (vs the previous behavior of
// using only the most recent feed tick).
func TestOrchestration_AggregatesSlashesAcrossClosedEpoch(t *testing.T) {
	env := newOrchestrationEnv(t)
	seedElection(env.elDb, 7, 1000, "alice", "bob")
	seedBond(env.balDb, "hive:alice", 999, 1_000_000)
	seedBond(env.balDb, "hive:bob", 999, 1_000_000)
	seedNodeBucket(env.se.LedgerSystem, 100, "test-accrue", 999)

	// 3 ticks, each adding 50 bps. Total = 150 bps on alice across the closed
	// epoch — captured only if the orchestration sums across snapshots.
	env.snaps.records = []pendulum_oracle.SnapshotRecord{
		{TickBlockHeight: 1010, WitnessSlashBps: []pendulum_oracle.WitnessSlashRecord{{Witness: "alice", Bps: 50}}},
		{TickBlockHeight: 1050, WitnessSlashBps: []pendulum_oracle.WitnessSlashRecord{{Witness: "alice", Bps: 50}}},
		{TickBlockHeight: 1090, WitnessSlashBps: []pendulum_oracle.WitnessSlashRecord{{Witness: "alice", Bps: 50}}},
	}

	if err := env.se.RunPendulumSettlementForTest(pendulumsettlement.BoundaryInfo{
		CurrentEpoch: 8, PreviousEpoch: 7, BlockHeight: 1100, Leader: "alice",
	}); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if len(env.bcast.calls) != 1 {
		t.Fatalf("expected one broadcast, got %d", len(env.bcast.calls))
	}
	payload := env.bcast.calls[0]
	if len(payload.Slashes) != 1 || payload.Slashes[0].Account != "hive:alice" || payload.Slashes[0].Bps != 150 {
		t.Fatalf("expected aggregated 150 bps slash on hive:alice, got %+v", payload.Slashes)
	}
}

// TestOrchestration_NilBroadcasterStillRunsMath confirms the test-harness
// path (no broadcaster) still executes orchestration cleanly without
// panicking — important because gql / contract test harnesses pass nil.
func TestOrchestration_NilBroadcasterStillRunsMath(t *testing.T) {
	env := newOrchestrationEnv(t)
	seedElection(env.elDb, 7, 1000, "alice")
	seedBond(env.balDb, "hive:alice", 999, 1_000_000)
	seedNodeBucket(env.se.LedgerSystem, 100, "test-accrue", 999)
	// Replace the broadcaster with a no-op nil-equivalent.
	env2 := state_engine.NewForPendulumSettlementTest(env.se.LedgerSystem, env.elDb, env.balDb, env.snaps, nil)

	if err := env2.RunPendulumSettlementForTest(pendulumsettlement.BoundaryInfo{
		CurrentEpoch: 8, PreviousEpoch: 7, BlockHeight: 1100, Leader: "alice",
	}); err != nil {
		t.Fatalf("expected nil error with nil broadcaster, got %v", err)
	}
}

// TestOrchestration_BroadcastErrorIsLoggedNotReturned guarantees a Hive RPC
// blip doesn't surface as a block-tick error and halt block processing.
func TestOrchestration_BroadcastErrorIsLoggedNotReturned(t *testing.T) {
	env := newOrchestrationEnv(t)
	env.bcast.returnErr = &fakeError{msg: "fake broadcast failure"}
	seedElection(env.elDb, 7, 1000, "alice")
	seedBond(env.balDb, "hive:alice", 999, 1_000_000)
	seedNodeBucket(env.se.LedgerSystem, 100, "test-accrue", 999)

	if err := env.se.RunPendulumSettlementForTest(pendulumsettlement.BoundaryInfo{
		CurrentEpoch: 8, PreviousEpoch: 7, BlockHeight: 1100, Leader: "alice",
	}); err != nil {
		t.Fatalf("broadcast error must not propagate, got %v", err)
	}
}

type fakeError struct{ msg string }

func (f *fakeError) Error() string { return f.msg }
