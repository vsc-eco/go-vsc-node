package election_proposer

import (
	"testing"

	"vsc-node/lib/dids"
	"vsc-node/lib/test_utils"
	"vsc-node/modules/common/consensusversion"
	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc/elections"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	"vsc-node/modules/db/vsc/poaseats"
	"vsc-node/modules/db/vsc/witnesses"

	ethBls "github.com/protolambda/bls12-381-util"
	"github.com/vsc-eco/hivego"
)

// POA election-path tests: the seat gate (A1), flat seat-weight (A3) and the
// churn cap (A4), driven through the real GenerateFullElection rather than
// through helper functions, because the properties that matter are properties
// of the assembled election — which accounts survive every filter, and what
// weights the emitted election carries.

// poaWitness builds a witness that passes the key filters and announces
// consensus 0.7.0, so it survives the version floor when the POA gates are
// driven by a 0.7.0 prevVersion.
func poaWitness(t *testing.T, account string, seedByte byte) witnesses.Witness {
	t.Helper()
	var seed [32]byte
	for i := range seed {
		seed[i] = seedByte
	}
	priv := dids.BlsPrivKey{}
	priv.Deserialize(&seed)
	pub, err := ethBls.SkToPk(&priv)
	if err != nil {
		t.Fatalf("SkToPk: %v", err)
	}
	did, err := dids.NewBlsDID(pub)
	if err != nil {
		t.Fatalf("NewBlsDID: %v", err)
	}
	consPoP, err := dids.GenerateBlsPoP(&priv, account)
	if err != nil {
		t.Fatalf("GenerateBlsPoP: %v", err)
	}
	kp := hivego.KeyPairFromBytes(seed[:])
	gwPoP, err := dids.GenerateGatewayKeyPoP(kp, account)
	if err != nil {
		t.Fatalf("GenerateGatewayKeyPoP: %v", err)
	}
	return witnesses.Witness{
		Account:         account,
		Enabled:         true,
		ProtocolVersion: 7,
		DidKeys: []witnesses.PostingJsonKeys{
			{CryptoType: "bls", Type: "consensus", Key: string(did), PoP: consPoP},
		},
		GatewayKey:    *kp.GetPublicKeyString(),
		GatewayKeyPoP: gwPoP,
	}
}

// poaHarness wires a proposer over mock DBs. stakes maps account -> consensus
// stake, so a test can give members deliberately lopsided stake and prove flat
// weight ignores it.
func poaHarness(t *testing.T, seats poaseats.PoaSeats, stakes map[string]int64) (ElectionProposer, *test_utils.MockElectionDb) {
	t.Helper()
	balanceDb := &test_utils.MockBalanceDb{
		BalanceRecords: map[string][]ledgerDb.BalanceRecord{},
	}
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
	ep := New(
		nil,
		&test_utils.MockWitnessDb{},
		elecDb,
		seats,
		nil,
		balanceDb,
		ct.DataLayer,
		nil,
		nil,
		systemconfig.MocknetConfig(),
		nil,
		nil,
	)
	return ep, elecDb
}

func memberAccounts(members []elections.ElectionMember) []string {
	out := make([]string, 0, len(members))
	for _, m := range members {
		out = append(out, m.Account)
	}
	return out
}

// A1: only ratified seats reach the committee. Without this, entry is
// permissionless — any account that announces itself a witness and holds enough
// stake is electable.
func TestPoaSeatGateExcludesUnseatedCandidates(t *testing.T) {
	seats := test_utils.NewMockPoaSeatsDb()
	seats.Seed("alice", "ubo-a", 10)
	seats.Seed("bob", "ubo-b", 10)
	seats.Seed("carol", "ubo-c", 10)
	// mallory is a fully valid, well-staked witness — and has no seat.

	ep, _ := poaHarness(t, seats, map[string]int64{
		"alice": 100, "bob": 100, "carol": 100, "mallory": 1_000_000,
	})

	list := []witnesses.Witness{
		poaWitness(t, "alice", 0x11),
		poaWitness(t, "bob", 0x22),
		poaWitness(t, "carol", 0x33),
		poaWitness(t, "mallory", 0x44),
	}
	_, data, err := ep.GenerateFullElection(list, 0, consensusversion.V0_7_0, 100)
	if err != nil {
		t.Fatalf("GenerateFullElection: %v", err)
	}
	got := memberAccounts(data.Members)
	if len(got) != 3 {
		t.Fatalf("members = %v, want the 3 seated accounts — an unseated witness reached the committee", got)
	}
	for _, m := range got {
		if m == "mallory" {
			t.Fatalf("members = %v — mallory holds no seat but the biggest stake, and stake alone must no longer buy a seat", got)
		}
	}
}

// ★ BR-1. An allowlist gate that fires against an empty registry deletes every
// candidate. The H-6 key gate did exactly that on mainnet at epoch 1699 and is
// still disabled today. The seat gate must go INERT instead, leaving candidacy
// exactly as it was until bootstrap seeding fills the registry.
func TestPoaSeatGateInertWhenRegistryEmpty(t *testing.T) {
	seats := test_utils.NewMockPoaSeatsDb() // empty
	ep, _ := poaHarness(t, seats, map[string]int64{"alice": 100, "bob": 100, "carol": 100})

	list := []witnesses.Witness{
		poaWitness(t, "alice", 0x11),
		poaWitness(t, "bob", 0x22),
		poaWitness(t, "carol", 0x33),
	}
	_, data, err := ep.GenerateFullElection(list, 0, consensusversion.V0_7_0, 100)
	if err != nil {
		t.Fatalf("GenerateFullElection: %v", err)
	}
	if len(data.Members) != 3 {
		t.Fatalf("members = %v, want all 3 — an empty registry emptied the committee, which is the epoch-1699 halt all over again",
			memberAccounts(data.Members))
	}
}

// A transient registry read must abort the election attempt, not silently
// produce a committee off a partial seat set. The next slot retries.
func TestPoaSeatGateFailsStopOnReadError(t *testing.T) {
	seats := test_utils.NewMockPoaSeatsDb()
	seats.Seed("alice", "ubo-a", 10)
	seats.FailReads = true

	ep, _ := poaHarness(t, seats, map[string]int64{"alice": 100, "bob": 100, "carol": 100})
	list := []witnesses.Witness{
		poaWitness(t, "alice", 0x11),
		poaWitness(t, "bob", 0x22),
		poaWitness(t, "carol", 0x33),
	}
	if _, _, err := ep.GenerateFullElection(list, 0, consensusversion.V0_7_0, 100); err == nil {
		t.Fatal("GenerateFullElection succeeded despite a failed seat-registry read — it would have built a committee from an unknown seat set")
	}
}

// Below the activation line the gate must not exist at all: an unseated witness
// is admitted exactly as it is today, so this binary and the current one produce
// identical elections.
func TestPoaSeatGateInertBelowActivation(t *testing.T) {
	seats := test_utils.NewMockPoaSeatsDb()
	seats.Seed("alice", "ubo-a", 10)

	ep, _ := poaHarness(t, seats, map[string]int64{"alice": 100, "bob": 100, "carol": 100})
	list := []witnesses.Witness{
		poaWitness(t, "alice", 0x11),
		poaWitness(t, "bob", 0x22),
		poaWitness(t, "carol", 0x33),
	}
	_, data, err := ep.GenerateFullElection(list, 0, consensusversion.V0_3_0, 100)
	if err != nil {
		t.Fatalf("GenerateFullElection: %v", err)
	}
	if len(data.Members) != 3 {
		t.Fatalf("members = %v, want all 3 — the seat gate bit below its activation version", memberAccounts(data.Members))
	}
}

// ★ A3, the property the whole design is stated in terms of: with flat weight a
// 2/3 threshold means two-thirds of SEATS. Here one member holds 10,000x the
// stake of the others and must still carry exactly one unit.
func TestPoaFlatWeightIgnoresStakeSize(t *testing.T) {
	seats := test_utils.NewMockPoaSeatsDb()
	for _, a := range []string{"alice", "bob", "carol"} {
		seats.Seed(a, "ubo-"+a, 10)
	}
	ep, _ := poaHarness(t, seats, map[string]int64{
		"alice": 1_000_000, "bob": 100, "carol": 100,
	})
	list := []witnesses.Witness{
		poaWitness(t, "alice", 0x11),
		poaWitness(t, "bob", 0x22),
		poaWitness(t, "carol", 0x33),
	}
	_, data, err := ep.GenerateFullElection(list, 0, consensusversion.V0_7_0, 100)
	if err != nil {
		t.Fatalf("GenerateFullElection: %v", err)
	}
	if len(data.Weights) != 3 {
		t.Fatalf("weights = %v, want 3 entries", data.Weights)
	}
	for i, w := range data.Weights {
		if w != params.PoaSeatWeight {
			t.Fatalf("weight[%d] (%s) = %d, want %d — a whale still outweighs its peers, which is the capture vector POA removes",
				i, data.Members[i].Account, w, params.PoaSeatWeight)
		}
	}
	// And the threshold that follows is a seat count, not a stake fraction.
	var total uint64
	for _, w := range data.Weights {
		total += w
	}
	if total != 3 {
		t.Fatalf("total weight = %d, want 3 (one per seat)", total)
	}
	if got := (2*total + 2) / 3; got != 2 {
		t.Fatalf("ceil(2/3) over 3 seats = %d, want 2", got)
	}
}

// The contrast case: below the activation line weight still tracks stake 1:1,
// proving the flattening is genuinely gated rather than always-on.
func TestWeightStillTracksStakeBelowActivation(t *testing.T) {
	seats := test_utils.NewMockPoaSeatsDb()
	ep, _ := poaHarness(t, seats, map[string]int64{
		"alice": 1_000_000, "bob": 100, "carol": 100,
	})
	list := []witnesses.Witness{
		poaWitness(t, "alice", 0x11),
		poaWitness(t, "bob", 0x22),
		poaWitness(t, "carol", 0x33),
	}
	_, data, err := ep.GenerateFullElection(list, 0, consensusversion.V0_3_0, 100)
	if err != nil {
		t.Fatalf("GenerateFullElection: %v", err)
	}
	var alice uint64
	for i, m := range data.Members {
		if m.Account == "alice" {
			alice = data.Weights[i]
		}
	}
	if alice != 1_000_000 {
		t.Fatalf("alice weight = %d below activation, want her raw stake 1000000 — the flattening is not gated", alice)
	}
}

// A4: the churn cap rate-limits new seats entering the committee, so an
// admission wave is visible and reactable rather than atomic. It is dead code
// today (its activation height is 0 on every network); POA activates it off the
// version gate instead.
func TestPoaChurnCapLimitsNewEntrantsPerElection(t *testing.T) {
	seats := test_utils.NewMockPoaSeatsDb()
	for _, a := range []string{"alice", "bob", "carol", "dave", "erin"} {
		seats.Seed(a, "ubo-"+a, 10)
	}
	ep, elecDb := poaHarness(t, seats, map[string]int64{
		"alice": 100, "bob": 100, "carol": 100, "dave": 100, "erin": 100,
	})
	// A previous ratified election with three members: dave and erin are the
	// "new" entrants this round.
	elecDb.Elections[0] = &elections.ElectionResult{
		ElectionCommonInfo: elections.ElectionCommonInfo{Epoch: 0, Type: "staked"},
		ElectionDataInfo: elections.ElectionDataInfo{
			Members: []elections.ElectionMember{
				{Account: "alice"}, {Account: "bob"}, {Account: "carol"},
			},
			Weights: []uint64{1, 1, 1},
		},
		BlockHeight: 50,
	}

	list := []witnesses.Witness{
		poaWitness(t, "alice", 0x11),
		poaWitness(t, "bob", 0x22),
		poaWitness(t, "carol", 0x33),
		poaWitness(t, "dave", 0x44),
		poaWitness(t, "erin", 0x55),
	}
	_, data, err := ep.GenerateFullElection(list, 0, consensusversion.V0_7_0, 100)
	if err != nil {
		t.Fatalf("GenerateFullElection: %v", err)
	}
	got := memberAccounts(data.Members)
	newcomers := 0
	for _, m := range got {
		if m == "dave" || m == "erin" {
			newcomers++
		}
	}
	if newcomers != 1 {
		t.Fatalf("members = %v: %d newcomers admitted, want 1 — the churn cap is inert, so a coordinated cohort could enter atomically",
			got, newcomers)
	}
	// Incumbents are never touched by the cap.
	for _, want := range []string{"alice", "bob", "carol"} {
		found := false
		for _, m := range got {
			if m == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("members = %v: incumbent %s was churned out — the cap must only defer NEW entrants", got, want)
		}
	}
}

// ★ THE STARVATION GUARD — the single most consequential test in this file.
//
// A partial bootstrap (or any short registry) leaves a registry that is
// NON-empty, so the inert-while-empty guard does not fire, but far too small.
// Applying the gate then deletes almost every candidate and the committee falls
// under the floor that gateway rotation, TSS signability and a valid election
// all require — which is the epoch-1699 halt reintroduced by the mechanism
// written to prevent it. The gate must decline to apply instead, costing one
// epoch of permissionless candidacy rather than the chain.
func TestPoaSeatGateRefusesToStarveTheCommittee(t *testing.T) {
	seats := test_utils.NewMockPoaSeatsDb()
	seats.Seed("alice", "ubo-a", 10) // exactly ONE seat: a partial bootstrap

	ep, _ := poaHarness(t, seats, map[string]int64{
		"alice": 100, "bob": 100, "carol": 100, "dave": 100,
	})
	list := []witnesses.Witness{
		poaWitness(t, "alice", 0x11),
		poaWitness(t, "bob", 0x22),
		poaWitness(t, "carol", 0x33),
		poaWitness(t, "dave", 0x44),
	}
	_, data, err := ep.GenerateFullElection(list, 0, consensusversion.V0_7_0, 100)
	if err != nil {
		t.Fatalf("GenerateFullElection: %v", err)
	}
	if len(data.Members) != 4 {
		t.Fatalf("members = %v (%d), want all 4 — the gate applied a one-seat registry and starved the committee, which is exactly the halt this guard exists to prevent",
			memberAccounts(data.Members), len(data.Members))
	}
}

// The guard must not become an excuse to never enforce: once the registry is
// large enough to leave a viable committee, the gate applies normally.
func TestPoaSeatGateAppliesOnceTheRegistryIsViable(t *testing.T) {
	seats := test_utils.NewMockPoaSeatsDb()
	for _, a := range []string{"alice", "bob", "carol", "dave"} {
		seats.Seed(a, "ubo-"+a, 10)
	}
	ep, _ := poaHarness(t, seats, map[string]int64{
		"alice": 100, "bob": 100, "carol": 100, "dave": 100, "mallory": 999_999,
	})
	list := []witnesses.Witness{
		poaWitness(t, "alice", 0x11),
		poaWitness(t, "bob", 0x22),
		poaWitness(t, "carol", 0x33),
		poaWitness(t, "dave", 0x44),
		poaWitness(t, "mallory", 0x55),
	}
	_, data, err := ep.GenerateFullElection(list, 0, consensusversion.V0_7_0, 100)
	if err != nil {
		t.Fatalf("GenerateFullElection: %v", err)
	}
	got := memberAccounts(data.Members)
	if len(got) != 4 {
		t.Fatalf("members = %v, want the 4 seated accounts", got)
	}
	for _, m := range got {
		if m == "mallory" {
			t.Fatalf("members = %v — the starvation guard let an unseated witness through even though the committee was viable without it", got)
		}
	}
}

// ★ Flat weight WITHOUT the seat gate is strictly worse than either regime:
// candidacy reverts to "anyone with MinStake", but every such candidate then
// carries the same weight as a vetted operator — so committee weight is bought
// at MinStake per seat instead of proportionally. Every POA election rule must
// therefore be gated on the registry being present, not on the version alone.
func TestPoaRulesApplyAsASetOrNotAtAll(t *testing.T) {
	ep, _ := poaHarness(t, nil, map[string]int64{
		"alice": 1_000_000, "bob": 100, "carol": 100,
	})
	list := []witnesses.Witness{
		poaWitness(t, "alice", 0x11),
		poaWitness(t, "bob", 0x22),
		poaWitness(t, "carol", 0x33),
	}
	_, data, err := ep.GenerateFullElection(list, 0, consensusversion.V0_7_0, 100)
	if err != nil {
		t.Fatalf("GenerateFullElection: %v", err)
	}
	var alice uint64
	for i, m := range data.Members {
		if m.Account == "alice" {
			alice = data.Weights[i]
		}
	}
	if alice == params.PoaSeatWeight {
		t.Fatal("weights were flattened on a node with no seat registry — candidacy is ungated but weight is flat, so committee weight costs MinStake per seat: cheaper than the stake-weighting POA replaced and free of the vetting it adds")
	}
	if alice != 1_000_000 {
		t.Fatalf("alice weight = %d, want her raw stake — POA rules must apply as a set or not at all", alice)
	}
}
