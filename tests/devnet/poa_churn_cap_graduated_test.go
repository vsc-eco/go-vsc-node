package devnet

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"vsc-node/modules/common/params"

	"go.mongodb.org/mongo-driver/bson"
)

// enableWitnessEverywhere is the inverse of disableWitnessEverywhere: it restores
// election eligibility by setting enabled=true on every node.
func enableWitnessEverywhere(t *testing.T, d *Devnet, ctx context.Context, account string, nodes int) int {
	t.Helper()
	client, err := d.mongoClient(ctx)
	if err != nil {
		t.Fatalf("mongo: %v", err)
	}
	defer client.Disconnect(ctx)
	total := 0
	for n := 1; n <= nodes; n++ {
		res, err := client.Database(d.nodeDbName(n)).Collection("witnesses").UpdateMany(ctx,
			bson.M{"account": account}, bson.M{"$set": bson.M{"enabled": true}})
		if err != nil {
			t.Fatalf("enabling %s on magi-%d: %v", account, n, err)
		}
		total += int(res.MatchedCount)
	}
	return total
}

// TestPoaChurnCapGraduatedEntry is scenario D7: a coordinated cohort must enter
// the committee GRADUATED, not atomically.
//
// The cap is what makes a suspicious admission wave visible and reactable
// instead of instantaneous. With PoaMaxNewMembersPerElection = 1 on devnet, two
// simultaneous entrants must take two elections to land, not one.
//
// ★ WHY THIS TEST IS VALID ON DEVNET, AND WOULD NOT BE ON MAINNET AS WRITTEN.
// The churn cap has an established-member exception: a returning member inside
// BondInclusionEstablishedGraceBlocks is not counted as a "new" entrant and
// re-enters immediately. Re-enabled witnesses are exactly such returners, so on
// a network where that exception is live this test would prove nothing and pass.
// It is valid here because `bondEstablished` is only populated when bondActive
// (election-proposer.go), and devnet sets BondInclusionActivationHeight = 0, so
// BondInclusionActive is false and the exception can never fire. That value is a
// DevnetConfig default rather than a test override, so it cannot be asserted
// honestly from here; the growth failure message names it explicitly instead, so
// a network where it changes yields a diagnosable failure rather than a silent
// pass.
//
// ★ It also exercises C6. The cap now resolves through PoaChurnCapActive AND
// poaSeatGateApplied; previously PoaChurnCapActive had no functional call site
// at all and the cap reached production only via poaActive, so the two could
// drift apart silently.
func TestPoaChurnCapGraduatedEntry(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}

	cfg := tssTestConfig()
	if cfg.SysConfigOverrides.ConsensusParams == nil {
		cfg.SysConfigOverrides.ConsensusParams = &params.ConsensusParams{}
	}
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorMajor = 0
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorConsensus = 7
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorEpoch = 1

	d, ctx := startDevnetNoKey(t, cfg, 45*time.Minute)

	for n := 1; n <= cfg.Nodes; n++ {
		nctx, cancel := context.WithTimeout(ctx, 8*time.Minute)
		err := d.waitForElectionEpoch(nctx, n, 2, 8*time.Minute)
		cancel()
		if err != nil {
			t.Fatalf("magi-%d never reached epoch 2: %v", n, err)
		}
	}

	// ---- PRECONDITION 1: POA active, full committee, all seated ----
	base, err := d.GetElectionGQL(ctx, 1, 2)
	if err != nil {
		t.Fatalf("reading election epoch 2: %v", err)
	}
	if !ccAllFlat(base.Weights) {
		t.Fatalf("PRECONDITION FAILED: epoch 2 weights=%v not flat. POA inert; the churn cap "+
			"resolves through poaSeatGateApplied and would never engage", base.Weights)
	}
	members := ccBareAccounts(base.Members)
	if len(members) < 5 {
		t.Fatalf("PRECONDITION FAILED: need a 5-member committee to remove 2 and stay at or above "+
			"MinMembers=3, got %d (%v)", len(members), members)
	}
	t.Logf("PRECONDITION OK: POA active, committee %v, flat weights", members)

	// ---- On the established-member exception ----
	// It cannot fire here because bondEstablished is only built when bondActive
	// (election-proposer.go) and devnet pins BondInclusionActivationHeight = 0.
	// That value lives in DevnetConfig, NOT in SysConfigOverrides, so there is no
	// honest way to assert it from the test config: reading the override struct
	// would just observe the zero value of a field nobody set and pass vacuously.
	// Instead the failure message below names both possible causes, so a future
	// network where bondActive is true produces a diagnosable failure rather than
	// a silent pass.

	// ---- Shrink the committee: disable 2 witnesses everywhere ----
	// 5 -> 3 keeps the committee at MinMembers, so an election still forms (D14
	// proved that below MinMembers no election forms at all).
	out := members[len(members)-2:]
	for _, acct := range out {
		n := disableWitnessEverywhere(t, d, ctx, acct, cfg.Nodes)
		t.Logf("disabled %s on %d witness rows across %d nodes", acct, n, cfg.Nodes)
	}

	shrunk := ccWaitElections(t, d, ctx, base.Epoch, 2, 12*time.Minute)
	shrunkMembers := ccBareAccounts(shrunk.Members)
	t.Logf("after disabling %v: epoch %d has %d members %v",
		out, shrunk.Epoch, len(shrunkMembers), shrunkMembers)
	if len(shrunkMembers) != len(members)-2 {
		t.Fatalf("PRECONDITION FAILED: committee is %d members after disabling 2, want %d. "+
			"Without a genuinely smaller previous committee there are no NEW entrants to cap, "+
			"and the rest of this test would be vacuous", len(shrunkMembers), len(members)-2)
	}

	// ---- Re-enable BOTH at once: a coordinated 2-member cohort ----
	for _, acct := range out {
		n := enableWitnessEverywhere(t, d, ctx, acct, cfg.Nodes)
		t.Logf("re-enabled %s on %d witness rows", acct, n)
	}

	// ---- THE ASSERTION: they enter one per election, not both at once ----
	//
	// effMaxNew = PoaMaxNewMembersPerElection = 1 on devnet. Note the cap yields
	// if deferring would breach MinMembers, but that cannot bite here: 5
	// candidates minus 1 deferred is 4, comfortably above 3.
	prev := len(shrunkMembers)
	sawGraduated := false
	epoch := shrunk.Epoch
	for step := 1; step <= 3; step++ {
		elec := ccWaitElections(t, d, ctx, epoch, 1, 10*time.Minute)
		epoch = elec.Epoch
		got := ccBareAccounts(elec.Members)
		grew := len(got) - prev
		t.Logf("epoch %d: %d members %v (grew by %d)", elec.Epoch, len(got), got, grew)

		if grew > 1 {
			t.Errorf("CHURN CAP DID NOT APPLY: committee grew by %d in a single election "+
				"(epoch %d, %d -> %d members). PoaMaxNewMembersPerElection is 1, so a "+
				"coordinated cohort must enter graduated. An atomic wave is exactly what the "+
				"cap exists to make impossible. TWO possible causes: the cap genuinely did not "+
				"engage (poaSeatGateApplied false, or PoaChurnCapActive false for this version), "+
				"OR bondActive is true on this network and the established-member exception "+
				"exempted the returning witnesses — check BondInclusionActivationHeight before "+
				"concluding the cap is broken", grew, elec.Epoch, prev, len(got))
			return
		}
		if grew == 1 {
			sawGraduated = true
		}
		// The deferral must never drop the committee under MinMembers (the F9 fix).
		if len(got) < 3 {
			t.Errorf("committee fell to %d members at epoch %d — deferring entrants must never "+
				"breach MinMembers; the cap is a rate limit, not a safety property",
				len(got), elec.Epoch)
		}
		prev = len(got)
		if len(got) == len(members) {
			t.Logf("cohort fully readmitted by epoch %d", elec.Epoch)
			break
		}
	}

	if !sawGraduated {
		t.Errorf("the committee never grew by exactly 1 across 3 elections after re-enabling a "+
			"2-member cohort (final size %d, want %d). Either the cap deferred both forever, or "+
			"nothing was ever readmitted — neither is graduated entry", prev, len(members))
	}
	if prev != len(members) {
		t.Errorf("committee ended at %d members, want the original %d — the deferred entrant was "+
			"never admitted in a later election, so the cap is blocking rather than rate-limiting",
			prev, len(members))
	}
}

// Helpers shared with the seat-gate scenarios, under this file's own names.

// ccBareAccounts strips the "hive:" prefix election members carry, so they can be
// compared against seat-registry accounts, which are stored bare.
func ccBareAccounts(members []string) []string {
	out := make([]string, 0, len(members))
	for _, m := range members {
		out = append(out, strings.TrimPrefix(m, "hive:"))
	}
	sort.Strings(out)
	return out
}

func ccAllFlat(weights []uint64) bool {
	for _, w := range weights {
		if w != params.PoaSeatWeight {
			return false
		}
	}
	return len(weights) > 0
}

// ccWaitElections blocks until node 1 has ingested `n` further election epochs,
// and returns the election it landed on.
func ccWaitElections(t *testing.T, d *Devnet, ctx context.Context, from uint64, n uint64, timeout time.Duration) *ElectionInfo {
	t.Helper()
	target := from + n
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := d.waitForElectionEpoch(wctx, 1, target, timeout); err != nil {
		t.Fatalf("magi-1 never reached election epoch %d: %v", target, err)
	}
	elec, err := d.GetElectionGQL(ctx, 1, target)
	if err != nil {
		t.Fatalf("reading election epoch %d: %v", target, err)
	}
	return elec
}
