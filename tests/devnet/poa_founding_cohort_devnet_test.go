package devnet

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// poaEnableWitnessAllNodes is the inverse of poaDisableWitnessAllNodes: it makes
// an account a candidate again on every node (witness rows are consensus input,
// so every node must see the same value).
func poaEnableWitnessAllNodes(t *testing.T, d *Devnet, ctx context.Context, account string, nodes int) int {
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

// TestPoaFoundingCohortAdmitsLateWitnesses (item 1, founding cohort): the election
// that switches POA on is built from the PREVIOUS version, so neither the seat gate
// nor the POA churn cap applies to it, and bootstrap then seats every member of it
// permanently. A witness that becomes a candidate just before the switch (on mainnet:
// stakes MinStake and announces) is seated without any admission vote, and there is
// no limit on how many do so at once.
//
// magi.test1-4 are the committee before the switch. magi.test5-7 are held out of it
// (witness disabled on every node), then made candidates right after the last
// pre-POA election, standing in for fresh stakers. The floor rises to 0.9 at epoch
// floorEpoch.
//
// Expected RED while the gap exists: magi.test5-7 hold bootstrap seats although no
// committee before the switch contained them. GREEN would seat only magi.test1-4 (or
// cap the newcomers at PoaMaxNewMembersPerElection=1).
func TestPoaFoundingCohortAdmitsLateWitnesses(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}
	const floorEpoch = 8
	cfg := tssTestConfig()
	cfg.Nodes = 7
	cfg.GenesisNode = 1
	cp := cfg.SysConfigOverrides.ConsensusParams
	cp.ConsensusVersionFloorMajor = 0
	cp.ConsensusVersionFloorConsensus = poaDevnetFloor()
	cp.ConsensusVersionFloorEpoch = floorEpoch
	d, ctx := startDevnetNoKey(t, cfg, 60*time.Minute)

	late := []int{5, 6, 7}
	acct := func(n int) string { return fmt.Sprintf("%s%d", cfg.WitnessPrefix, n) }
	bare := func(ms []string) []string {
		out := make([]string, len(ms))
		for i, m := range ms {
			out[i] = strings.TrimPrefix(m, "hive:")
		}
		return out
	}

	// Hold magi.test5-7 out of the committee from the start.
	if err := d.waitForElectionEpoch(ctx, 1, 1, 10*time.Minute); err != nil {
		t.Fatalf("no first election: %v", err)
	}
	for _, n := range late {
		if m := poaDisableWitnessAllNodes(t, d, ctx, acct(n), cfg.Nodes); m == 0 {
			t.Fatalf("PRECONDITION FAILED: no witness rows for %s", acct(n))
		}
	}
	t.Logf("disabled %v as witnesses on every node", late)

	// Wait for the last pre-POA election and check the late witnesses are not in it
	// (nor in the one before it).
	if err := d.waitForElectionEpoch(ctx, 1, floorEpoch-1, 20*time.Minute); err != nil {
		t.Fatalf("never reached epoch %d: %v", floorEpoch-1, err)
	}
	for _, ep := range []uint64{floorEpoch - 2, floorEpoch - 1} {
		mem, err := d.GetElectionMembers(ctx, 1, ep)
		if err != nil {
			t.Fatalf("members of epoch %d: %v", ep, err)
		}
		for _, n := range late {
			if contains(bare(mem), acct(n)) {
				t.Fatalf("PRECONDITION FAILED: %s is already in the pre-POA committee of epoch %d: %v", acct(n), ep, mem)
			}
		}
		t.Logf("pre-POA committee epoch %d: %v", ep, bare(mem))
	}
	if seats := pfMustSeats(t, d, ctx, 1); len(seats) != 0 {
		t.Fatalf("PRECONDITION FAILED: seat registry not empty before the switch: %s", pfSeatFingerprint(seats))
	}

	// Fresh candidates right before the switch.
	for _, n := range late {
		poaEnableWitnessAllNodes(t, d, ctx, acct(n), cfg.Nodes)
	}
	head, _ := getHeadBlock(d.HiveRPCEndpoint())
	t.Logf("re-enabled %v at block %d, after the epoch-%d election", late, head, floorEpoch-1)

	// The switch, plus one more election so any cap or gate would have bitten.
	for n := 1; n <= cfg.Nodes; n++ {
		if err := d.waitForElectionEpoch(ctx, n, floorEpoch+1, 15*time.Minute); err != nil {
			t.Fatalf("magi-%d never reached epoch %d: %v", n, floorEpoch+1, err)
		}
	}
	memT, _ := d.GetElectionMembers(ctx, 1, floorEpoch)
	memT1, _ := d.GetElectionMembers(ctx, 1, floorEpoch+1)
	t.Logf("transition committee epoch %d: %v", floorEpoch, bare(memT))
	t.Logf("first POA-gated committee epoch %d: %v", floorEpoch+1, bare(memT1))

	seats := pfMustSeats(t, d, ctx, 1)
	fp := pfSeatFingerprint(seats)
	t.Logf("seat registry (magi-1): %s", fp)
	for n := 2; n <= cfg.Nodes; n++ {
		if other := pfSeatFingerprint(pfMustSeats(t, d, ctx, n)); other != fp {
			t.Errorf("seat registry differs on magi-%d: %s", n, other)
		}
	}
	if len(seats) == 0 {
		t.Fatalf("PRECONDITION FAILED: bootstrap never seeded the seat registry")
	}

	var seatedLate []string
	for _, s := range seats {
		for _, n := range late {
			if s.Account == acct(n) {
				seatedLate = append(seatedLate, fmt.Sprintf("%s(bootstrap=%v, ubo=%q)", s.Account, s.Bootstrap, s.UboId))
			}
		}
	}
	if len(seatedLate) > 0 {
		t.Errorf("FOUNDING-COHORT: %d witness(es) that were in no committee before the switch hold permanent seats without an admission vote: %v (all seats: %s)", len(seatedLate), seatedLate, fp)
	} else {
		t.Logf("no late witness was seated")
	}
}
