package state_engine

// Schedule-memo pins for getScheduleForSlot. The memo is keyed by the
// (round, election) pair the schedule is a pure function of; these tests pin
// the three behaviours that keying must deliver:
//
//   - a mid-round election, followed by the re-seed case (a produce_block op
//     for a slot at or below the election's block_height validated AFTER the
//     election op), must NOT serve the old committee's schedule to later
//     slots of the round (the round-only key served it, and producers —
//     uncached GetSchedule — used the new committee, so blocks were skipped
//     until the round ended);
//   - the memo must retain its purpose: zero election reads per
//     produce_block op within a round with no election change;
//   - a fresh engine over a DB that already holds a mid-round election (a
//     restart) must seed its latest-election mirror from the DB and resolve
//     correctly.
//
// Heights follow the real CONSENSUS_SPECS (SlotLength 10, ScheduleLength
// 1200): round 2400 spans slots 2400..3590, and the mid-round election lands
// inside slot 2430's 10-block span at 2435.

import (
	"fmt"
	"testing"

	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/hive_blocks"

	"go.mongodb.org/mongo-driver/mongo"
)

// schedElectionDb implements the real GetElectionByHeight contract: the
// stored election with the greatest block_height strictly below the query
// height (rows are stored in ascending block_height, the replay order).
// Reads are counted so the memo's hit/miss behaviour can be pinned.
type schedElectionDb struct {
	elections.Elections
	rows  []elections.ElectionResult
	reads int
}

func (m *schedElectionDb) GetElectionByHeight(height uint64) (elections.ElectionResult, error) {
	m.reads++
	var best elections.ElectionResult
	found := false
	for _, r := range m.rows {
		if r.BlockHeight < height {
			best, found = r, true
		}
	}
	if !found {
		return elections.ElectionResult{}, fmt.Errorf("no election below %d: %w", height, mongo.ErrNoDocuments)
	}
	return best, nil
}

// store mirrors TxElectionResult.ExecuteTx's StoreElection +
// onElectionStored pair: the row lands in the DB and the engine is told the
// election was stored (which must also refresh the latest-election mirror
// without a DB read).
func (m *schedElectionDb) store(se *StateEngine, r elections.ElectionResult) {
	m.rows = append(m.rows, r)
	se.onElectionStored(r)
}

// schedHiveBlocks answers every GetBlock with an error so computeSchedule
// takes the deterministic default-seed path ("VSC.NETWORK" + round start).
type schedHiveBlocks struct {
	hive_blocks.HiveBlocks
}

func (schedHiveBlocks) GetBlock(uint64) (hive_blocks.HiveBlock, error) {
	return hive_blocks.HiveBlock{}, fmt.Errorf("no hive block: %w", mongo.ErrNoDocuments)
}

func schedMembers(accounts ...string) []elections.ElectionMember {
	ms := make([]elections.ElectionMember, 0, len(accounts))
	for _, a := range accounts {
		ms = append(ms, elections.ElectionMember{Account: a, Key: "bls-" + a})
	}
	return ms
}

func schedSlotAccount(t *testing.T, schedule []WitnessSlot, slotHeight uint64) string {
	t.Helper()
	for _, s := range schedule {
		if s.SlotHeight == slotHeight {
			return s.Account
		}
	}
	t.Fatalf("schedule has no entry for slot %d", slotHeight)
	return ""
}

// schedAccountsEqual compares two schedules by the account scheduled for each
// slot height.
func schedAccountsEqual(a, b []WitnessSlot) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// schedTestElections builds the disjoint-committee pair the tests rotate
// through: e0 stored below the round (epoch 1, alice/bob), e1 stored mid-round
// inside slot midSlot's span (epoch 2, carol/dave) — disjoint memberships so
// a stale schedule is provably distinguishable from a fresh one.
func schedTestElections(round, midSlot uint64) (e0, e1 elections.ElectionResult) {
	e0 = elections.ElectionResult{
		ElectionCommonInfo: elections.ElectionCommonInfo{Epoch: 1},
		ElectionDataInfo:   elections.ElectionDataInfo{Members: schedMembers("alice", "bob")},
	}
	e0.BlockHeight = round - 100
	e1 = elections.ElectionResult{
		ElectionCommonInfo: elections.ElectionCommonInfo{Epoch: 2},
		ElectionDataInfo:   elections.ElectionDataInfo{Members: schedMembers("carol", "dave")},
	}
	e1.BlockHeight = midSlot + CONSENSUS_SPECS.SlotLength/2
	return e0, e1
}

// schedEngine wires the minimal engine these paths need: the election stub
// (computeSchedule + the memo's mirror seed only read it) and the hive stub
// (deterministic default seed).
func schedEngine(db *schedElectionDb) *StateEngine {
	return &StateEngine{electionDb: db, hiveBlocks: schedHiveBlocks{}}
}

// TestGetScheduleForSlot_MidRoundElectionReSeed is the regression pin for the
// stale-memo bug: an election stored mid-round (inside slot midSlot's span)
// followed by a produce_block op for that same pre-election slot validated
// AFTER the election op used to re-cache the OLD committee's schedule under
// the same round key, so every later slot of the round hit the stale schedule
// while producers (uncached GetSchedule) used the new committee. The memo
// must instead notice its election is no longer the latest stored and
// recompute — and keep agreeing with the fresh read for the whole round.
func TestGetScheduleForSlot_MidRoundElectionReSeed(t *testing.T) {
	round := 2 * CONSENSUS_SPECS.ScheduleLength
	midSlot := round + 3*CONSENSUS_SPECS.SlotLength
	nextSlot := midSlot + CONSENSUS_SPECS.SlotLength
	e0, e1 := schedTestElections(round, midSlot)

	db := &schedElectionDb{rows: []elections.ElectionResult{e0}}
	se := schedEngine(db)

	// Slot `round` (round start): the first produce_block op of the round
	// builds the memo from e0 and must agree with the fresh read.
	if got, want := se.getScheduleForSlot(round), se.GetSchedule(round); !schedAccountsEqual(got, want) {
		t.Fatalf("slot %d (round start): memo != fresh read", round)
	}
	if acct := schedSlotAccount(t, se.getScheduleForSlot(round), round); acct != "alice" && acct != "bob" {
		t.Fatalf("slot %d must be scheduled to the e0 committee (alice/bob), got %q", round, acct)
	}

	// The mid-round election lands at e1.BlockHeight (inside midSlot's span):
	// the vsc.election_result handler stores it and calls onElectionStored.
	db.store(se, e1)

	// The re-seed: midSlot's produce_block op is validated AFTER the election
	// op (it landed late in its slot). GetElectionByHeight(midSlot) still
	// resolves e0 — the election activates only above its block_height — so
	// this op is correctly validated against e0's schedule; the memo is
	// rebuilt from e0 under the same round key.
	if got, want := se.getScheduleForSlot(midSlot), se.GetSchedule(midSlot); !schedAccountsEqual(got, want) {
		t.Fatalf("slot %d (pre-election, validated late): memo != fresh read", midSlot)
	}

	// nextSlot resolves e1. The round-only key returned the e0 schedule here
	// (the bug); the election-keyed memo must recompute and agree with the
	// fresh read, i.e. the new committee.
	got, want := se.getScheduleForSlot(nextSlot), se.GetSchedule(nextSlot)
	if !schedAccountsEqual(got, want) {
		t.Fatalf(
			"slot %d after mid-round election: stale memo\n  memo account %q (e0 schedule)\n  fresh account %q (e1 schedule)",
			nextSlot, schedSlotAccount(t, got, nextSlot), schedSlotAccount(t, want, nextSlot),
		)
	}
	if acct := schedSlotAccount(t, got, nextSlot); acct != "carol" && acct != "dave" {
		t.Fatalf("slot %d must be scheduled to the e1 committee (carol/dave), got %q", nextSlot, acct)
	}

	// And it must keep agreeing for the rest of the round (hits keyed by the
	// still-latest election), the window in which the stale memo skipped
	// every block.
	for s := nextSlot + CONSENSUS_SPECS.SlotLength; s < round+CONSENSUS_SPECS.ScheduleLength; s += CONSENSUS_SPECS.SlotLength {
		if got, want := se.getScheduleForSlot(s), se.GetSchedule(s); !schedAccountsEqual(got, want) {
			t.Fatalf("slot %d: memo != fresh read after mid-round election (memo %q, fresh %q)",
				s, schedSlotAccount(t, got, s), schedSlotAccount(t, want, s))
		}
	}
}

// TestGetScheduleForSlot_MemoHitsWithinRound pins the memo's purpose: within
// a round with no election change, exactly one election read is issued for
// the round-start build (the latest-election mirror seed is deferred to the
// first hit attempt), and every further produce_block op of the round is
// served from the memo — one further read in total.
func TestGetScheduleForSlot_MemoHitsWithinRound(t *testing.T) {
	round := 2 * CONSENSUS_SPECS.ScheduleLength
	e0, _ := schedTestElections(round, round+CONSENSUS_SPECS.SlotLength)

	db := &schedElectionDb{rows: []elections.ElectionResult{e0}}
	se := schedEngine(db)

	se.getScheduleForSlot(round)
	if db.reads != 1 {
		t.Fatalf("round-start op: election reads = %d, want 1 (build; mirror seed deferred to first hit)", db.reads)
	}

	for s := round + CONSENSUS_SPECS.SlotLength; s < round+CONSENSUS_SPECS.ScheduleLength; s += CONSENSUS_SPECS.SlotLength {
		se.getScheduleForSlot(s)
	}
	if db.reads != 2 {
		t.Fatalf("memo must serve the rest of the round on one seed read: reads = %d, want 2 (build + deferred seed)", db.reads)
	}
}

// TestGetScheduleForSlot_MemoRebuildsOnlyOnElectionChange pins the read
// pattern across the mid-round election + re-seed sequence: the mirror is
// maintained in memory by onElectionStored (no read), the re-seed from the
// pre-election slot and the rebuild from the post-election slot each cost
// exactly one build read, and the rest of the round hits the memo without
// further reads.
func TestGetScheduleForSlot_MemoRebuildsOnlyOnElectionChange(t *testing.T) {
	round := 2 * CONSENSUS_SPECS.ScheduleLength
	midSlot := round + 3*CONSENSUS_SPECS.SlotLength
	nextSlot := midSlot + CONSENSUS_SPECS.SlotLength
	e0, e1 := schedTestElections(round, midSlot)

	db := &schedElectionDb{rows: []elections.ElectionResult{e0}}
	se := schedEngine(db)

	se.getScheduleForSlot(round) // build = 1 read
	if db.reads != 1 {
		t.Fatalf("round-start op: election reads = %d, want 1", db.reads)
	}

	db.store(se, e1) // onElectionStored: no DB read
	if db.reads != 1 {
		t.Fatalf("onElectionStored must refresh the mirror without a DB read: reads = %d, want 1", db.reads)
	}

	se.getScheduleForSlot(midSlot) // re-seed build (+1)
	if db.reads != 2 {
		t.Fatalf("re-seed op: election reads = %d, want 2", db.reads)
	}

	se.getScheduleForSlot(nextSlot) // rebuild from the new election (+1; the mirror is already current from onElectionStored, so no seed read)
	if db.reads != 3 {
		t.Fatalf("post-election op: election reads = %d, want 3 (build + re-seed build + rebuild)", db.reads)
	}

	for s := nextSlot + CONSENSUS_SPECS.SlotLength; s < round+CONSENSUS_SPECS.ScheduleLength; s += CONSENSUS_SPECS.SlotLength {
		se.getScheduleForSlot(s)
	}
	if db.reads != 3 {
		t.Fatalf("memo must serve the rest of the round without election reads: reads = %d, want 3", db.reads)
	}
}

// TestGetScheduleForSlot_SeedsMirrorAfterRestart covers a fresh engine (empty
// memo, unseeded latest-election mirror) over a DB that already holds the
// mid-round election: the mirror must be seeded from the DB on first use, the
// first op of the round must resolve the new committee, and the rest of the
// round must hit the memo.
func TestGetScheduleForSlot_SeedsMirrorAfterRestart(t *testing.T) {
	round := 2 * CONSENSUS_SPECS.ScheduleLength
	midSlot := round + 3*CONSENSUS_SPECS.SlotLength
	nextSlot := midSlot + CONSENSUS_SPECS.SlotLength
	e0, e1 := schedTestElections(round, midSlot)

	db := &schedElectionDb{rows: []elections.ElectionResult{e0, e1}}
	se := schedEngine(db)

	got := se.getScheduleForSlot(nextSlot) // build = 1 read (mirror seed deferred to first hit)
	want := se.GetSchedule(nextSlot)       // oracle read (+1)
	if !schedAccountsEqual(got, want) {
		t.Fatalf(
			"slot %d after restart over a mid-round election: memo != fresh read\n  memo account %q\n  fresh account %q",
			nextSlot, schedSlotAccount(t, got, nextSlot), schedSlotAccount(t, want, nextSlot),
		)
	}
	if acct := schedSlotAccount(t, got, nextSlot); acct != "carol" && acct != "dave" {
		t.Fatalf("slot %d must be scheduled to the e1 committee (carol/dave), got %q", nextSlot, acct)
	}
	if db.reads != 2 {
		t.Fatalf("restart build + oracle: election reads = %d, want 2", db.reads)
	}

	oracleReads := 0
	for s := nextSlot + CONSENSUS_SPECS.SlotLength; s < round+CONSENSUS_SPECS.ScheduleLength; s += CONSENSUS_SPECS.SlotLength {
		oracleReads++
		if got, want := se.getScheduleForSlot(s), se.GetSchedule(s); !schedAccountsEqual(got, want) {
			t.Fatalf("slot %d: memo != fresh read after restart", s)
		}
	}
	// Each iteration must cost exactly one read — the GetSchedule oracle — plus
	// the one deferred mirror seed on the first hit attempt, so every
	// getScheduleForSlot call of the rest of the round hit the memo.
	if db.reads != 2+1+oracleReads {
		t.Fatalf("rest of round must hit the memo: reads = %d, want 2+1+%d oracle reads", db.reads, oracleReads)
	}
}
