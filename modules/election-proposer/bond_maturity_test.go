package election_proposer

import (
	"errors"
	"math"
	"slices"
	"testing"
	"vsc-node/modules/db/vsc/elections"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	"vsc-node/modules/db/vsc/witnesses"
)

// mockReplayReader returns a per-(account,height) hive_consensus balance from a
// step function: balances are the value at-or-after each listed height.
type mockReplayReader struct {
	// for each account, a sorted list of (fromHeight, balance) steps.
	steps map[string][]struct {
		from uint64
		bal  int64
	}
	// errAt, when non-zero, makes the read at exactly that height fail —
	// models a transient DB error at one sample (F4/H-10 fail-stop tests).
	errAt uint64
	// reverses, when set, are returned by ConsensusReverseCreditsInRange (X1).
	reverses []ledgerDb.LedgerRecord
	// reverseErr, when set, makes ConsensusReverseCreditsInRange fail.
	reverseErr bool
}

func (m *mockReplayReader) GetConsensusBalanceAt(account string, bh uint64) (int64, error) {
	if m.errAt != 0 && bh == m.errAt {
		return 0, errors.New("injected read error")
	}
	var bal int64
	for _, s := range m.steps[account] {
		if bh >= s.from {
			bal = s.bal
		}
	}
	return bal, nil
}

func (m *mockReplayReader) ConsensusReverseCreditsInRange(account string, start, end uint64) ([]ledgerDb.LedgerRecord, error) {
	if m.reverseErr {
		return nil, errors.New("injected reverse-credit read error")
	}
	out := make([]ledgerDb.LedgerRecord, 0, len(m.reverses))
	for _, r := range m.reverses {
		if r.BlockHeight >= start && r.BlockHeight <= end {
			out = append(out, r)
		}
	}
	return out, nil
}

func reader(account string, steps ...struct {
	from uint64
	bal  int64
}) *mockReplayReader {
	return &mockReplayReader{steps: map[string][]struct {
		from uint64
		bal  int64
	}{account: steps}}
}

type step = struct {
	from uint64
	bal  int64
}

func TestMaturedConsensusStake(t *testing.T) {
	const W = uint64(100)
	const H = uint64(1000)
	const samples = 8

	cases := []struct {
		name  string
		steps []step
		want  int64
	}{
		// Held >= the whole window: matured at the held value (min == held).
		{"matured_steady", []step{{0, 2_000_000}}, 2_000_000},
		// Staked AFTER the window opened (at 950): early samples (900..) read 0
		// → min 0 → NOT matured. This is the 3-day gate doing its job.
		{"fresh_stake_midwindow", []step{{950, 2_000_000}}, 0},
		// Staked exactly at/before window start and held: matured.
		{"staked_at_window_start", []step{{900, 2_000_000}}, 2_000_000},
		// Topped up mid-window: only the matured base counts (min == base).
		{"topup_midwindow", []step{{0, 2_000_000}, {950, 9_000_000}}, 2_000_000},
		// Started unstaking mid-window (balance dropped): min drops immediately.
		{"unstaking_midwindow", []step{{0, 5_000_000}, {950, 1_000_000}}, 1_000_000},
		// Never staked: 0.
		{"never_staked", nil, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := reader("hive:alice", c.steps...)
			got, err := maturedConsensusStake(r, "hive:alice", H, W, samples)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Fatalf("matured = %d, want %d", got, c.want)
			}
		})
	}
}

// F4/H-10: ANY sample read error must surface as an error (fail-stop), never
// as a silent 0 (which would evict an honest member on one node only and fork
// the election CID) and never as a skipped sample (which would weaken the gate).
func TestMaturedConsensusStake_ReadErrorFailsStop(t *testing.T) {
	const W, H = uint64(100), uint64(1000)
	r := reader("hive:alice", step{0, 2_000_000})
	// Fail the read at the election height itself (always sampled).
	r.errAt = H
	if got, err := maturedConsensusStake(r, "hive:alice", H, W, 8); err == nil {
		t.Fatalf("sample read error must fail-stop, got value %d with nil error", got)
	}
	// Same for the w==0 point-in-time path.
	if got, err := maturedConsensusStake(r, "hive:alice", H, 0, 0); err == nil {
		t.Fatalf("point-in-time read error must fail-stop, got value %d with nil error", got)
	}
	// And a nil reader is a wiring bug, not a zero.
	if got, err := maturedConsensusStake(nil, "hive:alice", H, W, 8); err == nil {
		t.Fatalf("nil reader must fail-stop, got value %d with nil error", got)
	}
}

// X1/H-17: a slashed-then-reversed node must NOT be exiled by the trailing min.
// alice held 5M the whole window, was slashed -3M at height 940, governance
// reversed +3M at height 970. Raw min would see the 2M dip (samples in
// [940,970)) → below a 3M MinStake → exiled. With the reverse add-back the dip
// samples are restored to 5M → matured.
func TestMaturedConsensusStake_ReverseSlashGrandfather(t *testing.T) {
	const W, H, sc = uint64(100), uint64(1000), 8
	// Balance step function: 5M held, drops to 2M at the slash (940), back to
	// 5M at the reverse (970) — exactly what GetConsensusBalanceAt would replay.
	r := reader("hive:alice", step{0, 5_000_000}, step{940, 2_000_000}, step{970, 5_000_000})
	r.reverses = []ledgerDb.LedgerRecord{{BlockHeight: 970, Amount: 3_000_000}}
	got, err := maturedConsensusStake(r, "hive:alice", H, W, sc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 5_000_000 {
		t.Fatalf("reverse-slash grandfather: exonerated node must read 5_000_000, got %d (raw min would be 2_000_000 → exiled)", got)
	}
	// Control: WITHOUT the reverse row, the dip stands (a genuine unstake, not a
	// reversed slash) → min 2M.
	r2 := reader("hive:alice", step{0, 5_000_000}, step{940, 2_000_000}, step{970, 5_000_000})
	got2, _ := maturedConsensusStake(r2, "hive:alice", H, W, sc)
	if got2 != 2_000_000 {
		t.Fatalf("no reverse row → genuine dip must stand, got %d want 2_000_000", got2)
	}
	// A reverse must NOT fast-track NEW stake: fresh stake 9M at 950 with a
	// reverse of 1M at 970 only adds 1M back to the pre-970 samples (which read
	// 0 before the fresh stake), so the min is the 1M add-back floor — NOT the
	// 9M fresh stake. Exact value (windowStart 900, samples include 901..998,1000;
	// earliest samples raw 0 + 1M reverse = 1M).
	fresh := reader("hive:bob", step{950, 9_000_000})
	fresh.reverses = []ledgerDb.LedgerRecord{{BlockHeight: 970, Amount: 1_000_000}}
	got3, _ := maturedConsensusStake(fresh, "hive:bob", H, W, sc)
	if got3 != 1_000_000 {
		t.Fatalf("reverse must not fast-track fresh stake: got %d, want exactly 1_000_000 (the add-back floor, not 9M)", got3)
	}

	// The `rc.BlockHeight > bh` height guard is LOAD-BEARING (audit static-F2):
	// a genuine unstake AFTER the reverse must NOT be masked by the add-back.
	// alice: held 5M, slashed -3M @940, reversed +3M @970, then UNSTAKED to 1M
	// @985. The reverse (970) must only be added back to samples BEFORE 970 — the
	// post-985 samples already reflect both the reverse and the unstake, so the
	// MIN must be the true current 1M, NOT an inflated 4M. (If the height guard
	// were dropped, the 970 reverse would be added to the 985+ samples too →
	// matured 4M against a real bond of 1M — a bypass.)
	unstakeAfter := reader("hive:alice",
		step{0, 5_000_000}, step{940, 2_000_000}, step{970, 5_000_000}, step{985, 1_000_000})
	unstakeAfter.reverses = []ledgerDb.LedgerRecord{{BlockHeight: 970, Amount: 3_000_000}}
	gotU, _ := maturedConsensusStake(unstakeAfter, "hive:alice", H, W, sc)
	if gotU != 1_000_000 {
		t.Fatalf("unstake-after-reverse must read the true current bond 1_000_000, got %d (height guard masked the unstake?)", gotU)
	}

	// STRONG boundary: a legit reverse + CONCURRENT fresh top-up. alice held 5M
	// the whole window, was slashed -3M at 940, added a FRESH 4M top-up at 960
	// (balance 6M), then the slash was reversed +3M at 970 (balance 9M). The
	// add-back must restore ONLY the slashed 5M base — never the un-matured 4M
	// top-up. Expected min = 5M (not 9M).
	mix := reader("hive:alice",
		step{0, 5_000_000}, step{940, 2_000_000}, step{960, 6_000_000}, step{970, 9_000_000})
	mix.reverses = []ledgerDb.LedgerRecord{{BlockHeight: 970, Amount: 3_000_000}}
	got4, _ := maturedConsensusStake(mix, "hive:alice", H, W, sc)
	if got4 != 5_000_000 {
		t.Fatalf("reverse must restore only the matured base, not a concurrent fresh top-up: got %d, want 5_000_000", got4)
	}
}

// X1 fail-stop: a reverse-credit read error must propagate (not silently 0).
func TestMaturedConsensusStake_ReverseReadErrorFailsStop(t *testing.T) {
	const W, H = uint64(100), uint64(1000)
	r := reader("hive:alice", step{0, 5_000_000})
	r.reverseErr = true
	if got, err := maturedConsensusStake(r, "hive:alice", H, W, 8); err == nil {
		t.Fatalf("reverse-credit read error must fail-stop, got value %d nil error", got)
	}
}

func TestMaturityWindowStartSaturates(t *testing.T) {
	// electionHeight < window must NOT underflow (Fear-1): clamp to 0.
	if got := maturityWindowStart(50, 100); got != 0 {
		t.Fatalf("windowStart(50,100) = %d, want 0 (saturating)", got)
	}
	if got := maturityWindowStart(0, 100); got != 0 {
		t.Fatalf("windowStart(0,100) = %d, want 0", got)
	}
	// w==0 disables the gate (single-point window).
	if got := maturityWindowStart(1000, 0); got != 0 {
		t.Fatalf("windowStart(1000,0) = %d, want 0 (gate disabled)", got)
	}
	if got := maturityWindowStart(1000, 100); got != 900 {
		t.Fatalf("windowStart(1000,100) = %d, want 900", got)
	}
}

func TestSampleMaturityWindowDeterministicAndBounded(t *testing.T) {
	// Same inputs → identical sample set (Constraint 3); all samples in (start,end]; end always present.
	a := sampleMaturityWindow(900, 1000, 8)
	b := sampleMaturityWindow(900, 1000, 8)
	if len(a) != len(b) {
		t.Fatalf("non-deterministic length %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("non-deterministic sample at %d: %d vs %d", i, a[i], b[i])
		}
	}
	last := a[len(a)-1]
	if last != 1000 {
		t.Fatalf("end height 1000 not the final sample (got %d)", last)
	}
	for _, bh := range a {
		if bh <= 900 || bh > 1000 {
			t.Fatalf("sample %d outside (900,1000]", bh)
		}
	}
	// Degenerate windows → single end sample, no panic/underflow.
	if got := sampleMaturityWindow(1000, 1000, 8); len(got) != 1 || got[0] != 1000 {
		t.Fatalf("degenerate equal window = %v, want [1000]", got)
	}
	if got := sampleMaturityWindow(0, 0, 8); got != nil {
		t.Fatalf("zero window = %v, want nil", got)
	}
}

// heightMapReader returns the exact balance set at a height (0 default) — used
// to model "spiky" balances (grinding) that the cumulative-step reader can't.
type heightMapReader struct {
	bal map[string]map[uint64]int64
}

func (m *heightMapReader) GetConsensusBalanceAt(account string, bh uint64) (int64, error) {
	return m.bal[account][bh], nil
}

func (m *heightMapReader) ConsensusReverseCreditsInRange(account string, start, end uint64) ([]ledgerDb.LedgerRecord, error) {
	return nil, nil
}

// B1: w==0 is the kill-switch → true point-in-time current balance, NOT the
// strictest genesis-dwell gate. A fresh topup must be returned in full.
func TestMaturedConsensusStake_GateDisabled(t *testing.T) {
	const H = uint64(1000)
	r := reader("hive:alice", step{0, 2_000_000}, step{990, 9_000_000})
	got, err := maturedConsensusStake(r, "hive:alice", H, 0 /*w*/, 8)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 9_000_000 {
		t.Fatalf("w==0 must be point-in-time current balance 9_000_000, got %d (B1: routing through the window would give 2_000_000 or 0)", got)
	}
	// brand-new stake at H, gate disabled → full current balance
	r2 := reader("hive:bob", step{H, 5_000_000})
	got2, err2 := maturedConsensusStake(r2, "hive:bob", H, 0, 8)
	if err2 != nil {
		t.Fatalf("unexpected error: %v", err2)
	}
	if got2 != 5_000_000 {
		t.Fatalf("w==0 fresh stake must count fully, got %d", got2)
	}
}

// B2: sampleCount <= 1 must NOT collapse the gate to a point-in-time read.
// With the floor, a fresh mid-window stake stays excluded.
func TestMaturedConsensusStake_SampleCountFloor(t *testing.T) {
	const W, H = uint64(100), uint64(1000)
	fresh := reader("hive:eve", step{960, 2_000_000}) // staked well inside the window
	for _, sc := range []int{0, 1} {
		got, err := maturedConsensusStake(fresh, "hive:eve", H, W, sc)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != 0 {
			t.Fatalf("sampleCount=%d must be floored to 2 (gate active) → fresh stake excluded, got %d (B2)", sc, got)
		}
	}
}

// N1: documented grinding limitation — high stake held ONLY at the deterministic
// sample heights passes. Locks the KNOWN behavior so a future change that alters
// it (and thus the dwell guarantee) is caught and the in-file comment updated.
func TestMaturedConsensusStake_GrindingKnownLimitation(t *testing.T) {
	const W, H, sc = uint64(100), uint64(1000), 8
	ws := maturityWindowStart(H, W)
	m := &heightMapReader{bal: map[string]map[uint64]int64{"hive:eve": {}}}
	for _, h := range sampleMaturityWindow(ws, H, sc) {
		m.bal["hive:eve"][h] = 5_000_000 // high ONLY at sample heights, 0 between
	}
	got, err := maturedConsensusStake(m, "hive:eve", H, W, sc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 5_000_000 {
		t.Fatalf("grinding is a KNOWN accepted limitation (should pass with 5_000_000), got %d — if this changed, update the in-file M1 note", got)
	}
}

// C7e: no panic / no overflow with electionHeight near MaxUint64.
func TestMaturedConsensusStake_NoOverflowHighHeight(t *testing.T) {
	const W = uint64(86_400)
	H := ^uint64(0) // MaxUint64
	r := reader("hive:alice", step{0, 2_000_000})
	got, err := maturedConsensusStake(r, "hive:alice", H, W, 8)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 2_000_000 {
		t.Fatalf("near-MaxUint64 height: matured = %d, want 2_000_000 (no overflow)", got)
	}
}

// Floor guard helpers (bond_gate.go) — pure parts.

func TestBondCommitteeFloor(t *testing.T) {
	// Mainnet shape: MinMembers=8, prev committee 20 → TSS term ceil(40/3)+1=15.
	if got := bondCommitteeFloor(8, 20); got != 15 {
		t.Fatalf("floor(8,20) = %d, want 15", got)
	}
	// Small prev committee: gateway floor 8 dominates.
	if got := bondCommitteeFloor(3, 5); got != 8 {
		t.Fatalf("floor(3,5) = %d, want 8 (gateway key floor)", got)
	}
	// Large MinMembers dominates.
	if got := bondCommitteeFloor(30, 20); got != 30 {
		t.Fatalf("floor(30,20) = %d, want 30", got)
	}
	// prev=0 (defensive): falls back to max(MinMembers, gateway floor).
	if got := bondCommitteeFloor(3, 0); got != 8 {
		t.Fatalf("floor(3,0) = %d, want 8", got)
	}
	// Exact thirds: prev=9 → ceil(18/3)+1 = 7 < 8 → 8.
	if got := bondCommitteeFloor(3, 9); got != 8 {
		t.Fatalf("floor(3,9) = %d, want 8", got)
	}
}

func TestSelectBondBackfillDeterministicOrder(t *testing.T) {
	w := func(acct string) witnesses.Witness { return witnesses.Witness{Account: acct} }
	cands := []bondBackfillCandidate{
		{witness: w("carol"), weight: 5, effective: 100, prevWeight: 50},
		{witness: w("alice"), weight: 5, effective: 300, prevWeight: 10},
		{witness: w("bob"), weight: 5, effective: 100, prevWeight: 80},
		{witness: w("dave"), weight: 5, effective: 100, prevWeight: 50},
	}
	got := selectBondBackfill(cands, 3)
	// Order: alice (effective 300) → bob (eff 100, prevW 80) → carol (eff 100,
	// prevW 50, account < dave).
	want := []string{"alice", "bob", "carol"}
	if len(got) != len(want) {
		t.Fatalf("selected %d, want %d", len(got), len(want))
	}
	for i, c := range got {
		if c.witness.Account != want[i] {
			t.Fatalf("position %d = %s, want %s", i, c.witness.Account, want[i])
		}
	}
	// need > len → all, same order; need <= 0 → none.
	if got := selectBondBackfill(slices.Clone(cands), 10); len(got) != 4 {
		t.Fatalf("need>len must return all candidates, got %d", len(got))
	}
	if got := selectBondBackfill(slices.Clone(cands), 0); got != nil {
		t.Fatalf("need=0 must return nil, got %v", got)
	}
}

func TestSelectChurnedOut(t *testing.T) {
	ents := []bondChurnEntrant{
		{account: "carol", effective: 100},
		{account: "alice", effective: 300},
		{account: "bob", effective: 100},
		{account: "dave", effective: 500},
	}
	// cap 2 → keep dave(500), alice(300); churn out bob & carol (eff 100 tie →
	// account asc keeps neither, both are below the kept set → both out).
	out := selectChurnedOut(slices.Clone(ents), 2)
	if len(out) != 2 {
		t.Fatalf("cap 2 of 4 → 2 churned, got %d", len(out))
	}
	if _, ok := out["bob"]; !ok {
		t.Fatalf("bob (eff 100) must be churned out")
	}
	if _, ok := out["carol"]; !ok {
		t.Fatalf("carol (eff 100) must be churned out")
	}
	if _, ok := out["dave"]; ok {
		t.Fatalf("dave (eff 500, highest) must be KEPT")
	}
	if _, ok := out["alice"]; ok {
		t.Fatalf("alice (eff 300) must be KEPT")
	}
	// Tie at the boundary is broken by account asc deterministically: 3 entrants
	// all eff 100, cap 2 → keep alice,bob (asc), churn carol.
	tie := []bondChurnEntrant{
		{account: "carol", effective: 100},
		{account: "bob", effective: 100},
		{account: "alice", effective: 100},
	}
	out2 := selectChurnedOut(slices.Clone(tie), 2)
	if len(out2) != 1 {
		t.Fatalf("cap 2 of 3 → 1 churned, got %d", len(out2))
	}
	if _, ok := out2["carol"]; !ok {
		t.Fatalf("carol (account-asc loser) must be churned, kept set {alice,bob}")
	}
	// cap >= len → nothing churned; cap <= 0 → disabled (nothing churned).
	if out := selectChurnedOut(slices.Clone(ents), 4); out != nil {
		t.Fatalf("cap==len must churn nothing, got %v", out)
	}
	if out := selectChurnedOut(slices.Clone(ents), 0); out != nil {
		t.Fatalf("cap 0 (disabled) must churn nothing, got %v", out)
	}
}

func TestBondWithinEstablishedGrace(t *testing.T) {
	const H, grace = uint64(1_000_000), uint64(400)
	// In grace: last membership 300 blocks ago (<= 400) → established.
	if !bondWithinEstablishedGrace(bondEstablishedInfo{lastMembershipHeight: H - 300, lastWeight: 5}, true, H, grace) {
		t.Fatal("within grace must be established")
	}
	// Exactly at the grace boundary (400 ago) → still established.
	if !bondWithinEstablishedGrace(bondEstablishedInfo{lastMembershipHeight: H - 400, lastWeight: 5}, true, H, grace) {
		t.Fatal("exactly at grace boundary must be established")
	}
	// Just past grace (401 ago) → expired.
	if bondWithinEstablishedGrace(bondEstablishedInfo{lastMembershipHeight: H - 401, lastWeight: 5}, true, H, grace) {
		t.Fatal("past grace must NOT be established")
	}
	// Not in map → false; grace 0 (disabled) → false; lastWeight 0 → false.
	if bondWithinEstablishedGrace(bondEstablishedInfo{}, false, H, grace) {
		t.Fatal("not-ok must be false")
	}
	if bondWithinEstablishedGrace(bondEstablishedInfo{lastMembershipHeight: H - 1, lastWeight: 5}, true, H, 0) {
		t.Fatal("grace 0 (disabled) must be false")
	}
	if bondWithinEstablishedGrace(bondEstablishedInfo{lastMembershipHeight: H - 1, lastWeight: 0}, true, H, grace) {
		t.Fatal("lastWeight 0 must be false")
	}
}

// The cap is the security boundary: the exemption can NEVER exceed current
// balance NOR last-ratified weight — so it only restores already-earned power,
// never fast-tracks new stake.
func TestBondEstablishedExemption_Cap(t *testing.T) {
	const H, grace = uint64(1_000_000), uint64(400)
	info := bondEstablishedInfo{lastMembershipHeight: H - 100, lastWeight: 5_000_000}
	// current >= last-ratified → exemption capped at last-ratified (new top-up
	// above last-ratified is NOT exempt — it must go through the window).
	if got := bondEstablishedExemption(info, true, H, grace, 9_000_000); got != 5_000_000 {
		t.Fatalf("top-up above last-ratified must cap at last-ratified 5_000_000, got %d", got)
	}
	// current < last-ratified (they unstaked some) → exemption = current (can't
	// exempt stake they no longer hold).
	if got := bondEstablishedExemption(info, true, H, grace, 2_000_000); got != 2_000_000 {
		t.Fatalf("partial-unstake must cap at current 2_000_000, got %d", got)
	}
	// expired (gone too long) → 0 (must re-mature).
	expired := bondEstablishedInfo{lastMembershipHeight: H - 500, lastWeight: 5_000_000}
	if got := bondEstablishedExemption(expired, true, H, grace, 5_000_000); got != 0 {
		t.Fatalf("expired exemption must be 0, got %d", got)
	}
	// not established → 0.
	if got := bondEstablishedExemption(bondEstablishedInfo{}, false, H, grace, 5_000_000); got != 0 {
		t.Fatalf("non-established must be 0, got %d", got)
	}
}

func TestBondEstablishedLookback(t *testing.T) {
	// mainnet: grace 403200 / interval 7200 = 56 + 16 buffer = 72.
	if got := bondEstablishedLookback(403_200, 7_200); got != 72 {
		t.Fatalf("lookback(403200,7200) = %d, want 72", got)
	}
	// interval 0 guarded (treated as 1) — no div-by-zero; bounded by the 2048 cap.
	if got := bondEstablishedLookback(403_200, 0); got != 2048 {
		t.Fatalf("lookback with interval 0 must clamp to 2048, got %d", got)
	}
	// tiny grace → at least 1 + buffer.
	if got := bondEstablishedLookback(40, 40); got != 17 {
		t.Fatalf("lookback(40,40) = %d, want 17", got)
	}
}

func TestBuildBondEstablishedMap_MostRecent(t *testing.T) {
	// GetPreviousElections returns epoch DESC. alice is in the two most-recent;
	// her record must reflect the MOST-RECENT membership height + weight. bob is
	// only in the older one. "hive:" prefix is normalized.
	mk := func(bh uint64, accts []string, weights []uint64) elections.ElectionResult {
		ms := make([]elections.ElectionMember, len(accts))
		for i, a := range accts {
			ms[i] = elections.ElectionMember{Account: a}
		}
		return elections.ElectionResult{
			ElectionDataInfo: elections.ElectionDataInfo{Members: ms, Weights: weights},
			BlockHeight:      bh,
		}
	}
	prevs := []elections.ElectionResult{
		mk(1000, []string{"hive:alice"}, []uint64{9}),
		mk(900, []string{"alice", "bob"}, []uint64{5, 3}),
	}
	m := buildBondEstablishedMap(prevs)
	if a := m["alice"]; a.lastMembershipHeight != 1000 || a.lastWeight != 9 {
		t.Fatalf("alice most-recent = {%d,%d}, want {1000,9}", a.lastMembershipHeight, a.lastWeight)
	}
	if b := m["bob"]; b.lastMembershipHeight != 900 || b.lastWeight != 3 {
		t.Fatalf("bob = {%d,%d}, want {900,3}", b.lastMembershipHeight, b.lastWeight)
	}
	if _, ok := m["carol"]; ok {
		t.Fatal("carol never served — must be absent")
	}
}

func TestUint64ToInt64Clamped(t *testing.T) {
	if got := uint64ToInt64Clamped(123); got != 123 {
		t.Fatalf("clamp(123) = %d", got)
	}
	if got := uint64ToInt64Clamped(^uint64(0)); got != math.MaxInt64 {
		t.Fatalf("clamp(MaxUint64) = %d, want MaxInt64", got)
	}
}
