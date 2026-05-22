package devnet

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// TestAuditFix_24_BannedWitnessExcludedFromNextElection is the end-to-end
// validation of audit #24. Pre-fix (*electionProposer).scoreMap() ran the
// under-75%-participation computation but no call site read BannedNodes —
// persistent under-participators stayed eligible.
//
// Post-fix GenerateFullElection consults scoreMap().BannedNodes and removes
// banned accounts from the witness list before the deterministic ordering.
// The filter is gated on scoreMapMinSamples (=500 in production, configurable
// via VSC_ELECTION_SCOREMAP_MIN_SAMPLES so tests can drive it in a few
// elections instead of an hour).
//
// Test scenario:
//   - 5 nodes, ElectionInterval=20 (60s), scoreMapMinSamples=20.
//   - Wait two elections to baseline so every node has at least one election
//     under its belt and the elections DB has prev/current data.
//   - Stop magi-3 entirely and let four elections pass — magi-3 misses every
//     block during that window (signing score = 0 against ~80 samples).
//   - Restart magi-3 so it can catch up indexing.
//   - Trigger one more election proposal.
//   - Assert: magi.test3 is NOT in the new election's Members list.
//
// Pre-fix: magi.test3 would still appear in the new committee.
// Post-fix: scoreMap returns BannedNodes=["magi.test3"] (it signed 0 / ~80
// samples << 75%), GenerateFullElection removes it.
func TestAuditFix_24_BannedWitnessExcludedFromNextElection(t *testing.T) {
	requireDocker(t)

	const electionInterval = 20

	cfg := DefaultConfig()
	cfg.Nodes = 5
	cfg.SkipFunding = true
	cfg.LogLevel = "info,election-proposer=info"
	cfg.SysConfigOverrides = &systemconfig.SysConfigOverrides{
		ConsensusParams: &params.ConsensusParams{
			ElectionInterval: electionInterval,
		},
	}
	cfg.MagiEnv = map[string]string{
		// Lower the floor so the scoreMap ban filter can fire after only a
		// handful of elections instead of 25. Production stays at 500.
		"VSC_ELECTION_SCOREMAP_MIN_SAMPLES": "20",
	}

	d, ctx := startDevnetNoKey(t, cfg, 25*time.Minute)

	// Baseline: let two elections happen with all nodes healthy.
	waitForBlock(t, d.HiveRPCEndpoint(), electionInterval*2+5, 4*time.Minute)

	const bannedNode = 3
	const bannedAccount = "magi.test3"

	t.Logf("stopping magi-%d to drive its block-signing score to 0...", bannedNode)
	if err := d.StopNode(ctx, bannedNode); err != nil {
		t.Fatalf("StopNode(%d): %v", bannedNode, err)
	}

	// Let 4 elections pass with magi-3 down. Each election = 20 blocks at 3s
	// = 60s, so 4 elections ≈ 4 minutes. During this window magi-3 signs
	// zero blocks while the other four nodes sign normally → magi-3's
	// score in the next scoreMap call is 0 against ~80 samples → banned.
	head, _ := getHeadBlock(d.HiveRPCEndpoint())
	bannedWindowEnd := ((head / electionInterval) + 4) * electionInterval
	waitForBlock(t, d.HiveRPCEndpoint(), bannedWindowEnd+5, 6*time.Minute)

	t.Logf("restarting magi-%d so it can index the catchup window...", bannedNode)
	if err := d.StartNode(ctx, bannedNode); err != nil {
		t.Fatalf("StartNode(%d): %v", bannedNode, err)
	}
	// Allow time to catch up on the missed blocks before the next election
	// proposal is built.
	time.Sleep(30 * time.Second)

	// Wait for at least one more election proposal AFTER the catchup window.
	head, _ = getHeadBlock(d.HiveRPCEndpoint())
	nextElectionBlock := ((head / electionInterval) + 2) * electionInterval
	waitForBlock(t, d.HiveRPCEndpoint(), nextElectionBlock+5, 4*time.Minute)
	time.Sleep(15 * time.Second) // drain async proposer

	// Read the latest election from Mongo and assert magi.test3 is excluded.
	members := readLatestElectionMembers(t, d.MongoURI())
	t.Logf("latest election members: %v", members)

	for _, m := range members {
		if m == bannedAccount {
			dumpNodeLogs(t, d, ctx, cfg.Nodes, 30)
			t.Fatalf("audit #24: expected %q to be excluded from next election (scoreMap ban), but it's still a member", bannedAccount)
		}
	}

	// Also assert at least one healthy node logged the ban skip with the
	// expected account. (Best-effort — the proposer is a specific witness
	// per slot, so only that node's logs show the ban.)
	saw := false
	for i := 1; i <= cfg.Nodes; i++ {
		if i == bannedNode {
			continue
		}
		logs, err := d.Logs(ctx, fmt.Sprintf("magi-%d", i))
		if err != nil {
			continue
		}
		if strings.Contains(logs, "election.skip banned witness") &&
			strings.Contains(logs, bannedAccount) {
			t.Logf("audit #24: ban filter fired on magi-%d for %q", i, bannedAccount)
			saw = true
			break
		}
	}
	if !saw {
		t.Logf("note: no node logged 'election.skip banned witness' for %q — "+
			"exclusion may have happened via the GenerateFullElection deletion path "+
			"without a per-witness log line (acceptable; member-list absence already "+
			"proves the fix is wired in)", bannedAccount)
	}
}

// readLatestElectionMembers fetches the highest-epoch election from the
// elections collection and returns the member accounts in order. Used by
// the #24 test to confirm post-ban committee composition.
func readLatestElectionMembers(t *testing.T, mongoURI string) []string {
	t.Helper()
	client, err := mongo.Connect(context.Background(), options.Client().ApplyURI(mongoURI))
	if err != nil {
		t.Fatalf("connect mongo: %v", err)
	}
	defer client.Disconnect(context.Background())

	coll := client.Database("magi-1").Collection("elections")
	opts := options.FindOne().SetSort(bson.M{"epoch": -1})
	res := coll.FindOne(context.Background(), bson.M{}, opts)
	if res.Err() != nil {
		t.Fatalf("FindOne elections: %v", res.Err())
	}
	var doc struct {
		Epoch   uint64 `bson:"epoch"`
		Members []struct {
			Account string `bson:"account"`
		} `bson:"members"`
	}
	if err := res.Decode(&doc); err != nil {
		t.Fatalf("decode election doc: %v", err)
	}
	t.Logf("latest election epoch=%d, %d members", doc.Epoch, len(doc.Members))
	out := make([]string, 0, len(doc.Members))
	for _, m := range doc.Members {
		out = append(out, m.Account)
	}
	return out
}
