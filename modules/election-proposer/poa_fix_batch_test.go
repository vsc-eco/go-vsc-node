package election_proposer

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/common/consensusversion"
	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc/consensus_state"
	"vsc-node/modules/db/vsc/elections"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	"vsc-node/modules/db/vsc/witnesses"
)

// POA-2 (0.9.0): when the seat gate would starve the committee, every seat is
// kept and unseated candidates are admitted only up to MinMembers, highest
// matured stake first. Below 0.9.0 the gate is skipped and every staker enters
// (TestPoaSeatGateRefusesToStarveTheCommittee pins that and must keep passing).

// w9 is poaWitness announcing consensus 0.9.0, so it survives the version floor
// when the election is driven by a 0.9.0 prevVersion (a 0.7.0 announcement is
// filtered out there and every assertion would run on an empty committee).
func w9(t *testing.T, account string, seedByte byte) witnesses.Witness {
	t.Helper()
	w := poaWitness(t, account, seedByte)
	w.ProtocolVersion = 9
	return w
}

func TestSelectPoaTopUpRanksIncumbentsThenStakeThenName(t *testing.T) {
	rank := map[string]int64{"bob": 300, "carol": 200, "dave": 200, "eve": 50}
	got := selectPoaTopUp([]string{"eve", "dave", "carol", "bob"}, nil, rank, 2)
	if _, ok := got["bob"]; !ok || len(got) != 2 {
		t.Fatalf("top-up picked %v, want bob plus the name-first of the 200 tie", got)
	}
	if _, ok := got["carol"]; !ok {
		t.Fatalf("tie at 200 must break by account name (carol < dave), got %v", got)
	}
	if n := len(selectPoaTopUp([]string{"bob"}, nil, rank, 0)); n != 0 {
		t.Fatalf("need 0 admitted %d", n)
	}
	if n := len(selectPoaTopUp([]string{"bob"}, nil, rank, -3)); n != 0 {
		t.Fatalf("negative need admitted %d", n)
	}
	if n := len(selectPoaTopUp([]string{"bob", "eve"}, nil, rank, 5)); n != 2 {
		t.Fatalf("need above the candidate count admitted %d, want all 2", n)
	}
	// Previous-committee members come before any stake: eve (50) outranks bob (300).
	inc := map[string]bool{"eve": true}
	if got := selectPoaTopUp([]string{"eve", "dave", "carol", "bob"}, inc, rank, 1); len(got) != 1 {
		t.Fatalf("incumbent pick = %v, want exactly eve", got)
	} else if _, ok := got["eve"]; !ok {
		t.Fatalf("incumbent pick = %v, want eve (previous committee) over bob (more stake)", got)
	}
}

func TestPoa2_StarvedCommitteeIsToppedUpToTheFloorAt090(t *testing.T) {
	floor := systemconfig.MocknetConfig().ConsensusParams().MinMembers
	if floor < 2 {
		t.Skipf("mocknet MinMembers %d leaves nothing to top up", floor)
	}
	seats := test_utils.NewMockPoaSeatsDb()
	seats.Seed("alice", "ubo-a", 10) // one seat: the gate alone would starve

	// Unseated stakers with distinct stake so the ranking is visible.
	stakes := map[string]int64{"alice": 100, "bob": 900, "carol": 800, "dave": 700, "eve": 600, "frank": 500}
	ep, _ := poaHarness(t, seats, stakes)
	list := []witnesses.Witness{
		w9(t, "alice", 0x11), w9(t, "bob", 0x22), w9(t, "carol", 0x33),
		w9(t, "dave", 0x44), w9(t, "eve", 0x55), w9(t, "frank", 0x66),
	}
	_, data, err := ep.GenerateFullElection(list, 0, consensusversion.V0_9_0, 100)
	if err != nil {
		t.Fatalf("GenerateFullElection: %v", err)
	}
	got := memberAccounts(data.Members)
	if len(got) != floor {
		t.Fatalf("members = %v (%d), want exactly the floor %d: every staker entering is the POA-2 open door", got, len(got), floor)
	}
	if !slices.Contains(got, "alice") {
		t.Fatalf("members = %v: the one real seat was dropped", got)
	}
	want := []string{"bob", "carol", "dave", "eve", "frank"}[:floor-1] // highest stake first
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Fatalf("members = %v, want the %d highest-stake unseated (%v)", got, floor-1, want)
		}
	}
	for _, w := range data.Weights {
		if w != params.PoaSeatWeight {
			t.Fatalf("weights = %v: top-up members must carry flat seat weight like seats", data.Weights)
		}
	}
}

// The top-up must not let richer newcomers push out members of the previous
// committee: they already hold key shares, and the churn cap cannot defer
// anyone once the committee is exactly at the floor.
func TestPoa2_TopUpKeepsIncumbentsOverRicherNewcomersAt090(t *testing.T) {
	floor := systemconfig.MocknetConfig().ConsensusParams().MinMembers
	if floor != 3 {
		t.Skipf("fixture assumes mocknet MinMembers 3, got %d", floor)
	}
	seats := test_utils.NewMockPoaSeatsDb()
	seats.Seed("alice", "ubo-a", 10)
	stakes := map[string]int64{"alice": 100, "u1": 100, "u2": 100, "a1": 900, "a2": 800}
	ep, elecDb := poaHarness(t, seats, stakes)
	elecDb.Elections[0] = &elections.ElectionResult{
		ElectionCommonInfo: elections.ElectionCommonInfo{Epoch: 0, Type: "staked"},
		ElectionDataInfo: elections.ElectionDataInfo{
			Members: []elections.ElectionMember{{Account: "alice"}, {Account: "u1"}, {Account: "u2"}},
			Weights: []uint64{1, 1, 1},
		},
		BlockHeight: 50,
	}
	list := []witnesses.Witness{
		w9(t, "alice", 0x11), w9(t, "u1", 0x22), w9(t, "u2", 0x33), w9(t, "a1", 0x44), w9(t, "a2", 0x55),
	}
	_, data, err := ep.GenerateFullElection(list, 0, consensusversion.V0_9_0, 100)
	if err != nil {
		t.Fatalf("GenerateFullElection: %v", err)
	}
	got := memberAccounts(data.Members)
	slices.Sort(got)
	if !slices.Equal(got, []string{"alice", "u1", "u2"}) {
		t.Fatalf("members = %v, want [alice u1 u2]: the previous committee must be kept before richer newcomers", got)
	}
}

// The top-up must pick from candidates that survive the FINAL version floor. In
// an epoch where a pinned floor rises, the highest-stake unseated candidate
// (bob) announces below it. Picking before the floor delete would admit bob,
// lose him to the floor, and leave the committee short of MinMembers, which
// aborts the election on every retry of that epoch.
func TestPoa2_TopUpPicksSurviveARisingVersionFloor(t *testing.T) {
	sconf := systemconfig.MocknetConfig()
	floor := sconf.ConsensusParams().MinMembers
	if floor != 3 {
		t.Skipf("fixture assumes mocknet MinMembers 3, got %d", floor)
	}
	override := filepath.Join(t.TempDir(), "sysconfig.json")
	if err := os.WriteFile(override, []byte(`{"consensusParams":{"consensusVersionFloorEpoch":1,"consensusVersionFloorMajor":0,"consensusVersionFloorConsensus":10}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := sconf.LoadOverrides(override); err != nil {
		t.Fatal(err)
	}
	seats := test_utils.NewMockPoaSeatsDb()
	seats.Seed("alice", "ubo-a", 10)
	stakes := map[string]int64{"alice": 100, "bob": 900, "carol": 800, "dave": 700, "erin": 600}
	ep, elecDb := poaHarnessWith(t, seats, stakes, sconf)
	elecDb.Elections[0] = &elections.ElectionResult{ // makes this election epoch 1, where the pin applies
		ElectionCommonInfo: elections.ElectionCommonInfo{Epoch: 0, Type: "staked"},
		ElectionDataInfo: elections.ElectionDataInfo{
			Members: []elections.ElectionMember{{Account: "alice"}, {Account: "carol"}, {Account: "dave"}, {Account: "erin"}},
			Weights: []uint64{1, 1, 1, 1},
		},
		BlockHeight: 50,
	}
	w10 := func(account string, seed byte) witnesses.Witness {
		w := poaWitness(t, account, seed)
		w.ProtocolVersion = 10
		return w
	}
	list := []witnesses.Witness{
		w10("alice", 0x11), w9(t, "bob", 0x22), w10("carol", 0x33), w10("dave", 0x44), w10("erin", 0x55),
	}
	_, data, err := ep.GenerateFullElection(list, 0, consensusversion.V0_9_0, 100)
	if err != nil {
		t.Fatalf("GenerateFullElection: %v", err)
	}
	got := memberAccounts(data.Members)
	if slices.Contains(got, "bob") {
		t.Fatalf("members = %v: bob announces 0.9 under a pinned 0.10 floor and must not be seated", got)
	}
	if len(got) < floor {
		t.Fatalf("members = %v (%d): below MinMembers %d, the election aborts on every retry of this epoch", got, len(got), floor)
	}
	for _, want := range []string{"alice", "carol", "dave"} { // the seat, then the two highest-stake survivors
		if !slices.Contains(got, want) {
			t.Fatalf("members = %v, want %s", got, want)
		}
	}
}

// poaHarnessWith is poaHarness with a caller-supplied system config.
func poaHarnessWith(t *testing.T, seats *test_utils.MockPoaSeatsDb, stakes map[string]int64, sconf systemconfig.SystemConfig) (ElectionProposer, *test_utils.MockElectionDb) {
	t.Helper()
	balanceDb := &test_utils.MockBalanceDb{BalanceRecords: map[string][]ledgerDb.BalanceRecord{}}
	for acct, amt := range stakes {
		balanceDb.BalanceRecords["hive:"+acct] = []ledgerDb.BalanceRecord{
			{Account: "hive:" + acct, BlockHeight: 0, HIVE_CONSENSUS: amt},
		}
	}
	elecDb := &test_utils.MockElectionDb{
		Elections:         map[uint64]*elections.ElectionResult{},
		ElectionsByHeight: map[uint64]elections.ElectionResult{},
	}
	ct := test_utils.NewContractTest()
	ep := New(nil, &test_utils.MockWitnessDb{}, elecDb, seats, nil, balanceDb, ct.DataLayer, nil, nil, sconf, nil, nil)
	return ep, elecDb
}

// POA-9: the starvation check used to count seats among the candidates BEFORE
// the stake filter. Three seats pass it (MinMembers 3), one of them has no
// stake, the stake filter removes it, and the committee comes out at 2: the
// election is aborted on every retry. At 0.9.0 the decision is taken on the
// survivors, so the committee is topped up to the floor instead.
func TestPoa9_SeatLosingItsStakeIsToppedUpNotStalledAt090(t *testing.T) {
	if systemconfig.MocknetConfig().ConsensusParams().MinMembers != 3 {
		t.Skip("fixture assumes mocknet MinMembers 3")
	}
	seats := test_utils.NewMockPoaSeatsDb()
	for _, a := range []string{"alice", "bob", "carol"} {
		seats.Seed(a, "ubo-"+a, 10)
	}
	stakes := map[string]int64{"alice": 100, "bob": 100, "carol": 0, "u1": 500, "u2": 300}
	for _, tc := range []struct {
		ver  consensusversion.Version
		want []string
	}{
		{consensusversion.V0_9_0, []string{"alice", "bob", "u1"}}, // topped up on the survivors
		{consensusversion.V0_7_0, []string{"alice", "bob"}},       // the POA-9 short committee
	} {
		ep, elecDb := poaHarness(t, seats, stakes)
		// Steady state: a previous staked election, so this one is "staked" too.
		elecDb.Elections[0] = &elections.ElectionResult{
			ElectionCommonInfo: elections.ElectionCommonInfo{Epoch: 0, Type: "staked"},
			ElectionDataInfo: elections.ElectionDataInfo{
				Members: []elections.ElectionMember{{Account: "alice"}, {Account: "bob"}, {Account: "carol"}},
				Weights: []uint64{1, 1, 1},
			},
			BlockHeight: 50,
		}
		mk := poaWitness
		if tc.ver.Consensus >= 9 {
			mk = w9
		}
		list := []witnesses.Witness{mk(t, "alice", 0x11), mk(t, "bob", 0x22), mk(t, "carol", 0x33), mk(t, "u1", 0x44), mk(t, "u2", 0x55)}
		_, data, err := ep.GenerateFullElection(list, 0, tc.ver, 100)
		if err != nil {
			t.Fatalf("%s: GenerateFullElection: %v", tc.ver.Format(), err)
		}
		got := memberAccounts(data.Members)
		slices.Sort(got)
		if !slices.Equal(got, tc.want) {
			t.Fatalf("%s: members = %v, want %v", tc.ver.Format(), got, tc.want)
		}
	}
}

// The normal path is unchanged at 0.9.0: with enough staked seats the gate
// applies and every unseated candidate is excluded, however much stake it has.
func TestPoa9_GateStillExcludesUnseatedWhenSeatsSufficeAt090(t *testing.T) {
	seats := test_utils.NewMockPoaSeatsDb()
	for _, a := range []string{"alice", "bob", "carol"} {
		seats.Seed(a, "ubo-"+a, 10)
	}
	stakes := map[string]int64{"alice": 100, "bob": 100, "carol": 100, "rich": 9000}
	ep, _ := poaHarness(t, seats, stakes)
	list := []witnesses.Witness{w9(t, "alice", 0x11), w9(t, "bob", 0x22), w9(t, "carol", 0x33), w9(t, "rich", 0x44)}
	_, data, err := ep.GenerateFullElection(list, 0, consensusversion.V0_9_0, 100)
	if err != nil {
		t.Fatalf("GenerateFullElection: %v", err)
	}
	got := memberAccounts(data.Members)
	slices.Sort(got)
	if !slices.Equal(got, []string{"alice", "bob", "carol"}) {
		t.Fatalf("members = %v, want only the three seats", got)
	}
}

// With the seat decision deferred, the candidate list still holds unseated
// stakers when the version-floor readiness ratio is computed. Unseated
// witnesses on an old version must not be able to hold back a floor rise the
// seats are ready for: readiness runs over the seats when enough of them are
// candidates.
func TestPoa9_ReadinessRunsOverSeatsWhenTheyFormTheCommittee(t *testing.T) {
	floor, target := v(0, 9), v(0, 10)
	seatsWs, wm := wlist(5, 5, target, floor) // a..e: seats, all on target
	var list []witnesses.Witness
	list = append(list, seatsWs...)
	for i := 0; i < 5; i++ { // five unseated witnesses on the old version
		acct := "z" + string(rune('a'+i))
		list = append(list, witnesses.Witness{Account: acct, ProtocolVersion: floor.Consensus})
		wm[acct] = 1
	}
	seats := map[string]struct{}{"a": {}, "b": {}, "c": {}, "d": {}, "e": {}}
	props := []consensus_state.VersionProposal{prop(0, 10, 1, 0, "a")}

	if got := resolveVersionFloor(floor, 2, 100, nil, props, list, wm, nil, num, den); got.Cmp(floor) != 0 {
		t.Fatalf("setup: the full list should hold the floor back (5 of 10 ready), got %s", got.Format())
	}
	rl := poaReadinessList(list, seats, 3)
	if len(rl) != 5 {
		t.Fatalf("readiness list = %d candidates, want the 5 seats", len(rl))
	}
	if got := resolveVersionFloor(floor, 2, 100, nil, props, rl, wm, nil, num, den); got.Cmp(target) != 0 {
		t.Fatalf("over the seats the floor should rise to %s, got %s", target.Format(), got.Format())
	}
	// Too few seats to form the committee: every candidate counts, as before.
	if got := poaReadinessList(list, map[string]struct{}{"a": {}}, 3); len(got) != len(list) {
		t.Fatalf("with 1 seat (< MinMembers) the readiness list = %d, want all %d", len(got), len(list))
	}
	if got := poaReadinessList(list, nil, 3); len(got) != len(list) {
		t.Fatal("gate not deferred: the list must pass through unchanged")
	}
}
