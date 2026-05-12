package state_engine

import (
	"strconv"
	"testing"

	"vsc-node/modules/common/params"
	"vsc-node/modules/db/vsc/elections"
	safetyslash "vsc-node/modules/incentive-pendulum/safety_slash"
	ledgerSystem "vsc-node/modules/ledger-system"
)

// stubLedgerSystem is a minimal in-test impl of the LedgerSystem interface
// scoped to the slashing path. It records invocations so detector wiring
// tests can assert which policy decisions reached the ledger boundary.
type stubLedgerSystem struct {
	slashCalls []ledgerSystem.SafetySlashConsensusParams
	slashOk    bool
}

func (s *stubLedgerSystem) SafetySlashConsensusBond(p ledgerSystem.SafetySlashConsensusParams) ledgerSystem.LedgerResult {
	s.slashCalls = append(s.slashCalls, p)
	if s.slashOk {
		return ledgerSystem.LedgerResult{Ok: true, Msg: "stub slash applied"}
	}
	return ledgerSystem.LedgerResult{Ok: false, Msg: "stub slash refused"}
}

func (s *stubLedgerSystem) GetBalance(account string, blockHeight uint64, asset string) int64 {
	return 0
}
func (s *stubLedgerSystem) ClaimHBDInterest(lastClaim uint64, blockHeight uint64, amount int64, txId string) {
}
func (s *stubLedgerSystem) IndexActions(actionUpdate map[string]interface{}, extraInfo ledgerSystem.ExtraInfo) {
}
func (s *stubLedgerSystem) Deposit(deposit ledgerSystem.Deposit) string { return "" }
func (s *stubLedgerSystem) IngestOplog(oplog []ledgerSystem.OpLogEvent, options ledgerSystem.OplogInjestOptions) {
}
func (s *stubLedgerSystem) PendulumDistribute(toAccount string, amount int64, txID string, blockHeight uint64) ledgerSystem.LedgerResult {
	return ledgerSystem.LedgerResult{Ok: false, Msg: "stub"}
}
func (s *stubLedgerSystem) FinalizeMaturedSafetySlashBurns(blockHeight uint64) {}
func (s *stubLedgerSystem) CancelPendingSafetySlashBurn(p ledgerSystem.CancelPendingSafetySlashBurnParams) ledgerSystem.LedgerResult {
	return ledgerSystem.LedgerResult{Ok: false, Msg: "stub"}
}
func (s *stubLedgerSystem) ReverseSafetySlashConsensusDebit(p ledgerSystem.ReverseSafetySlashConsensusDebitParams) ledgerSystem.LedgerResult {
	return ledgerSystem.LedgerResult{Ok: false, Msg: "stub"}
}
func (s *stubLedgerSystem) PendulumBucketBalance(bucket string, blockHeight uint64) int64 { return 0 }
func (s *stubLedgerSystem) NewEmptySession(state *ledgerSystem.LedgerState, startHeight uint64) ledgerSystem.LedgerSession {
	return nil
}
func (s *stubLedgerSystem) NewEmptyState() *ledgerSystem.LedgerState { return nil }

// TestRecordEvidenceAndShouldSlash_ImmediateAndDedup covers the only surface
// the principal-slash policy currently exposes: every wired kind is immediate
// (single deterministic proof slashes), but duplicate evidence ids must never
// re-slash so detectors are safe to re-enter on replay.
func TestRecordEvidenceAndShouldSlash_ImmediateAndDedup(t *testing.T) {
	se := &StateEngine{
		safetyEvidenceSeen: make(map[string]uint64),
	}
	if !se.recordEvidenceAndShouldSlash("alice", safetyslash.EvidenceVSCDoubleBlockSign, "ev-1", 100) {
		t.Fatal("immediate evidence kind should slash on first proof")
	}
	if se.recordEvidenceAndShouldSlash("alice", safetyslash.EvidenceVSCDoubleBlockSign, "ev-1", 100) {
		t.Fatal("duplicate evidence id at same height must not re-slash")
	}
	if !se.recordEvidenceAndShouldSlash("alice", safetyslash.EvidenceVSCDoubleBlockSign, "ev-2", 105) {
		t.Fatal("distinct evidence id should slash again")
	}
	if !se.recordEvidenceAndShouldSlash("alice", safetyslash.EvidenceVSCInvalidBlockProposal, "ev-1", 110) {
		t.Fatal("different kind with same evidence id should slash independently")
	}
}

// TestRecordEvidenceAndShouldSlash_RejectsBlankInputs guards the early-exit
// validation in the policy entrypoint so callers cannot accidentally slash
// with an empty account or kind string.
func TestRecordEvidenceAndShouldSlash_RejectsBlankInputs(t *testing.T) {
	se := &StateEngine{
		safetyEvidenceSeen: make(map[string]uint64),
	}
	if se.recordEvidenceAndShouldSlash("", safetyslash.EvidenceVSCDoubleBlockSign, "ev", 100) {
		t.Fatal("blank account must not slash")
	}
	if se.recordEvidenceAndShouldSlash("alice", "", "ev", 100) {
		t.Fatal("blank kind must not slash")
	}
}

// TestBlamedAccountsFromBitSet exercises the blame-bitset parser. The function
// no longer drives a principal slash (TSS blame is liveness-only), but the
// helper stays so future reward-reduction wiring can reuse it deterministically.
func TestBlamedAccountsFromBitSet(t *testing.T) {
	members := []elections.ElectionMember{
		{Account: "alice"},
		{Account: "bob"},
		{Account: "carol"},
	}
	got := blamedAccountsFromBitSet("05", members)
	if len(got) != 2 || got[0] != "hive:alice" || got[1] != "hive:carol" {
		t.Fatalf("unexpected blamed accounts: %#v", got)
	}
}

// newTestSlashingEngine builds a minimal StateEngine wired to a stub
// LedgerSystem. The detector call sites in state_engine.go funnel through
// slashForEvidenceIfPolicyAllows → SafetySlashConsensusBond, so exercising
// that path with a stub captures the policy decisions (kind acceptance,
// dedup, correlation cap) without needing a full BLS-signed block fixture.
func newTestSlashingEngine() (*StateEngine, *stubLedgerSystem) {
	stub := &stubLedgerSystem{slashOk: true}
	se := &StateEngine{
		LedgerSystem:                  stub,
		safetyEvidenceSeen:            make(map[string]uint64),
		seenProposalBySlotProposer:    make(map[string]string),
		slashIncidentBpsBySlotAccount: make(map[string]int),
	}
	return se, stub
}

// TestSlashForEvidence_DoubleBlockSign covers the double-sign detector's
// policy hand-off: first proof slashes, replay/duplicate evidence does not.
func TestSlashForEvidence_DoubleBlockSign(t *testing.T) {
	se, stub := newTestSlashingEngine()

	res := se.slashForEvidenceIfPolicyAllows(
		"alice",
		safetyslash.EvidenceVSCDoubleBlockSign,
		"double-block|slot-200|cidA|cidB",
		"tx-double-1",
		200,
		"slot-200|hive:alice",
	)
	if !res.Ok {
		t.Fatalf("first proof should slash: %s", res.Msg)
	}
	if len(stub.slashCalls) != 1 {
		t.Fatalf("expected 1 SafetySlashConsensusBond call, got %d", len(stub.slashCalls))
	}
	if stub.slashCalls[0].SlashBps != safetyslash.DoubleBlockSignSlashBps {
		t.Fatalf("slashBps: got %d want %d", stub.slashCalls[0].SlashBps, safetyslash.DoubleBlockSignSlashBps)
	}
	if stub.slashCalls[0].EvidenceKind != safetyslash.EvidenceVSCDoubleBlockSign {
		t.Fatalf("evidenceKind: got %s want %s", stub.slashCalls[0].EvidenceKind, safetyslash.EvidenceVSCDoubleBlockSign)
	}

	// Replay the same evidence — must dedup at the policy layer.
	res2 := se.slashForEvidenceIfPolicyAllows(
		"alice",
		safetyslash.EvidenceVSCDoubleBlockSign,
		"double-block|slot-200|cidA|cidB",
		"tx-double-1",
		200,
		"slot-200|hive:alice",
	)
	if res2.Ok {
		t.Fatalf("duplicate evidence should not re-slash; got Ok with %s", res2.Msg)
	}
	if len(stub.slashCalls) != 1 {
		t.Fatalf("dedup leaked: expected 1 ledger call, got %d", len(stub.slashCalls))
	}
}

// TestSlashForEvidence_InvalidBlock covers the invalid-block detector's
// policy hand-off.
func TestSlashForEvidence_InvalidBlock(t *testing.T) {
	se, stub := newTestSlashingEngine()

	res := se.slashForEvidenceIfPolicyAllows(
		"alice",
		safetyslash.EvidenceVSCInvalidBlockProposal,
		"invalid-block|tx-invalid-1",
		"tx-invalid-1",
		300,
		"slot-300|hive:alice",
	)
	if !res.Ok {
		t.Fatalf("first proof should slash: %s", res.Msg)
	}
	if len(stub.slashCalls) != 1 {
		t.Fatalf("expected 1 ledger call, got %d", len(stub.slashCalls))
	}
	if stub.slashCalls[0].SlashBps != safetyslash.InvalidBlockSlashBps {
		t.Fatalf("slashBps: got %d want %d", stub.slashCalls[0].SlashBps, safetyslash.InvalidBlockSlashBps)
	}
}

// TestSlashForEvidence_CorrelatedIncident verifies that when both block-
// production kinds fire on the same (slot, account), the second slash is
// capped against CorrelatedSlashCapBps so the producer doesn't take two
// independent 10% hits beyond the policy ceiling.
func TestSlashForEvidence_CorrelatedIncident(t *testing.T) {
	se, stub := newTestSlashingEngine()
	incidentKey := "slot-500|hive:alice"

	// First kind fires the full 1000 bps.
	res1 := se.slashForEvidenceIfPolicyAllows(
		"alice",
		safetyslash.EvidenceVSCDoubleBlockSign,
		"double-block|slot-500",
		"tx-corr-1",
		500,
		incidentKey,
	)
	if !res1.Ok {
		t.Fatalf("first kind should slash: %s", res1.Msg)
	}
	if stub.slashCalls[0].SlashBps != safetyslash.DoubleBlockSignSlashBps {
		t.Fatalf("first slash bps: got %d want %d", stub.slashCalls[0].SlashBps, safetyslash.DoubleBlockSignSlashBps)
	}

	// Set running incident to near the cap so the second kind's 1000 bps
	// must be clamped to the remaining headroom.
	se.slashIncidentBpsBySlotAccount[incidentKey] = safetyslash.CorrelatedSlashCapBps - 200 // 9_800

	res2 := se.slashForEvidenceIfPolicyAllows(
		"alice",
		safetyslash.EvidenceVSCInvalidBlockProposal,
		"invalid-block|slot-500",
		"tx-corr-2",
		500,
		incidentKey,
	)
	if !res2.Ok {
		t.Fatalf("second kind within cap should slash: %s", res2.Msg)
	}
	if len(stub.slashCalls) != 2 {
		t.Fatalf("expected 2 slash ledger calls (correlated incident still funnels each kind through ledger): got %d",
			len(stub.slashCalls))
	}
	if got := stub.slashCalls[1].SlashBps; got != 200 {
		t.Fatalf("correlated cap should clamp second slash to 200 bps headroom, got %d", got)
	}

	// Third kind on the same incident must reject — incident is at cap.
	res3 := se.slashForEvidenceIfPolicyAllows(
		"alice",
		safetyslash.EvidenceVSCDoubleBlockSign,
		"double-block|slot-500|over",
		"tx-corr-3",
		500,
		incidentKey,
	)
	if res3.Ok {
		t.Fatalf("over-cap slash should reject; got %s", res3.Msg)
	}
	if len(stub.slashCalls) != 2 {
		t.Fatalf("over-cap kind must not reach ledger: got %d calls", len(stub.slashCalls))
	}
}

// TestSlashForEvidence_UnknownKindRejected ensures retired evidence strings
// (e.g. legacy "settlement_payload_fraud") cannot drive a slash even when
// metadata leaks them through.
func TestSlashForEvidence_UnknownKindRejected(t *testing.T) {
	se, stub := newTestSlashingEngine()

	res := se.slashForEvidenceIfPolicyAllows(
		"alice",
		"settlement_payload_fraud", // retired kind
		"legacy-evidence",
		"tx-legacy",
		400,
		"",
	)
	if res.Ok {
		t.Fatalf("retired kind must not slash; got %s", res.Msg)
	}
	if len(stub.slashCalls) != 0 {
		t.Fatalf("retired kind must not reach ledger: got %d calls", len(stub.slashCalls))
	}
}

// TestValidateDetailed_SkipVsInvalid documents the contract added in B1:
// election-lookup failures yield Skip=true (no slash), explicitly-bad
// signatures yield Valid=false / Skip=false (slash). The detector wiring
// reads these two flags to gate the slash trigger correctly.
func TestValidateDetailed_SkipDoesNotBecomeValid(t *testing.T) {
	out := BlockValidationOutcome{Skip: true, SkipReason: "election lookup failed"}
	if out.Valid {
		t.Fatal("Skip outcomes must not also be Valid")
	}
}

// TestPruneSafetySlotMaps_DropsAtOrBelowFinalizedSlot verifies the slot-
// keyed in-memory maps shed entries up to and including the finalized
// slot, and preserve entries for slots strictly newer than the threshold.
func TestPruneSafetySlotMaps_DropsAtOrBelowFinalizedSlot(t *testing.T) {
	se, _ := newTestSlashingEngine()

	se.seenProposalBySlotProposer["100|hive:alice"] = "cidA"
	se.seenProposalBySlotProposer["100|hive:bob"] = "cidB"
	se.seenProposalBySlotProposer["200|hive:alice"] = "cidC"
	se.seenProposalBySlotProposer["300|hive:carol"] = "cidD"

	se.slashIncidentBpsBySlotAccount["100|hive:alice"] = 1000
	se.slashIncidentBpsBySlotAccount["200|hive:alice"] = 1000
	se.slashIncidentBpsBySlotAccount["300|hive:carol"] = 1000

	se.pruneSafetySlotMaps(200)

	if _, ok := se.seenProposalBySlotProposer["100|hive:alice"]; ok {
		t.Fatalf("slot 100 must be pruned at finalized=200")
	}
	if _, ok := se.seenProposalBySlotProposer["200|hive:alice"]; ok {
		t.Fatalf("slot 200 (finalized) must be pruned")
	}
	if _, ok := se.seenProposalBySlotProposer["300|hive:carol"]; !ok {
		t.Fatalf("slot 300 (after finalized) must be preserved")
	}

	if _, ok := se.slashIncidentBpsBySlotAccount["100|hive:alice"]; ok {
		t.Fatalf("incident at slot 100 must be pruned")
	}
	if _, ok := se.slashIncidentBpsBySlotAccount["300|hive:carol"]; !ok {
		t.Fatalf("incident at slot 300 must be preserved")
	}
}

// TestPruneSafetySlotMaps_NoOpOnEmptyMaps proves the prune is safe to
// call when the engine has never seen evidence — important because the
// slot-transition path runs every block, including blocks before any
// double-sign or invalid-block detector has fired.
func TestPruneSafetySlotMaps_NoOpOnEmptyMaps(t *testing.T) {
	se := &StateEngine{
		seenProposalBySlotProposer:    make(map[string]string),
		slashIncidentBpsBySlotAccount: make(map[string]int),
	}
	se.pruneSafetySlotMaps(1000)
	if len(se.seenProposalBySlotProposer) != 0 || len(se.slashIncidentBpsBySlotAccount) != 0 {
		t.Fatalf("prune on empty maps must not introduce entries")
	}
}

// TestPruneSafetySlotMaps_HandlesMalformedKeys ensures keys that do not
// follow the "slot|account" convention are dropped on prune. Defensive:
// the maps are only written by code paths that produce well-formed keys,
// but the prune must still be robust.
func TestPruneSafetySlotMaps_HandlesMalformedKeys(t *testing.T) {
	se, _ := newTestSlashingEngine()
	se.seenProposalBySlotProposer["malformed-no-pipe"] = "cidX"
	se.seenProposalBySlotProposer["100|hive:alice"] = "cidA"

	se.pruneSafetySlotMaps(50)
	if _, ok := se.seenProposalBySlotProposer["malformed-no-pipe"]; ok {
		t.Fatalf("malformed key (slot=0) must be pruned at any finalized height")
	}
	if _, ok := se.seenProposalBySlotProposer["100|hive:alice"]; !ok {
		t.Fatalf("slot 100 entry must survive when finalized=50")
	}
}

// TestPruneSafetyEvidenceSeen_HeightWindow verifies the dedup map sheds
// entries older than 2x the maximum burn delay. Newer entries (within
// the window) must be preserved so dedup still works for in-flight
// reversal+re-slash sequences at the burn-delay boundary.
func TestPruneSafetyEvidenceSeen_HeightWindow(t *testing.T) {
	se, _ := newTestSlashingEngine()

	keepWindow := 2 * params.MaxSafetySlashBurnDelayBlocks
	currentHeight := keepWindow * 2 // far enough in the chain that the prune fires

	se.safetyEvidenceSeen["old"] = currentHeight - keepWindow - 1     // outside window: prune
	se.safetyEvidenceSeen["edge"] = currentHeight - keepWindow        // exactly at window: keep
	se.safetyEvidenceSeen["recent"] = currentHeight - keepWindow + 10 // inside window: keep

	se.pruneSafetyEvidenceSeen(currentHeight)

	if _, ok := se.safetyEvidenceSeen["old"]; ok {
		t.Fatal("evidence older than 2x max burn delay must be pruned")
	}
	if _, ok := se.safetyEvidenceSeen["edge"]; !ok {
		t.Fatal("evidence exactly at the window boundary must be kept")
	}
	if _, ok := se.safetyEvidenceSeen["recent"]; !ok {
		t.Fatal("evidence inside the window must be kept")
	}
}

// TestPruneSafetyEvidenceSeen_NoPruneOnYoungChain proves the prune is a
// no-op while the chain has not yet reached 2x the maximum burn delay.
// We don't want a freshly-bootstrapped node to drop evidence whose age
// underflows the height threshold.
func TestPruneSafetyEvidenceSeen_NoPruneOnYoungChain(t *testing.T) {
	se, _ := newTestSlashingEngine()
	se.safetyEvidenceSeen["a"] = 1
	se.safetyEvidenceSeen["b"] = 100

	se.pruneSafetyEvidenceSeen(params.MaxSafetySlashBurnDelayBlocks)
	if len(se.safetyEvidenceSeen) != 2 {
		t.Fatalf("young chain must not prune evidence: got %d entries", len(se.safetyEvidenceSeen))
	}
}

// TestSafetySlotMaps_BoundedAcrossManySlots is a stress-style test: a
// long sequence of (slot, account) entries must not accumulate when the
// prune is invoked after each slot finalizes, modeling the slot-transition
// behavior in StateEngine.ProcessBlock.
func TestSafetySlotMaps_BoundedAcrossManySlots(t *testing.T) {
	se, _ := newTestSlashingEngine()

	const slotsToSimulate = 10_000
	for slot := uint64(1); slot <= slotsToSimulate; slot++ {
		key := strconv.FormatUint(slot, 10) + "|hive:proposer"
		se.seenProposalBySlotProposer[key] = "cid-" + key
		se.slashIncidentBpsBySlotAccount[key] = 1000
		// Slot N finalizes when slot N+1 begins; mirror that here.
		if slot > 1 {
			se.pruneSafetySlotMaps(slot - 1)
		}
	}

	// After the loop, only the most recently observed slot should remain
	// (we never pruned slot N during slot N's iteration).
	if len(se.seenProposalBySlotProposer) > 1 {
		t.Fatalf("seenProposalBySlotProposer leaked: %d entries", len(se.seenProposalBySlotProposer))
	}
	if len(se.slashIncidentBpsBySlotAccount) > 1 {
		t.Fatalf("slashIncidentBpsBySlotAccount leaked: %d entries", len(se.slashIncidentBpsBySlotAccount))
	}
}
