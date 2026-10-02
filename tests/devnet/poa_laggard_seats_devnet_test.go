package devnet

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// TestPoaLaggardIncumbentsLoseSeatsAtTheRise (item 9, mixed versions at the floor
// rise): the election that raises the floor to 0.9 drops every incumbent still on an
// older binary, and bootstrap seats only the members of that election. An incumbent
// that upgrades one epoch late is no longer a seat, so under the seat gate it stays
// out of every later committee until an admission vote brings it back (one per
// election under the churn cap). On mainnet, with exactly 12 of 17 upgraded at the
// rise, 5 incumbents would be out and the BTC key would have zero spare signers.
//
// magi.test6 and magi.test7 are built from TSS_LAGGARD_SOURCE (announce 0.8 until
// /tmp/upgraded exists). The floor rises to 0.9 at floorEpoch; they upgrade right
// after it.
//
// Expected RED while the gap exists: after upgrading, magi.test6/7 hold no seat and
// are in no committee for several elections.
func TestPoaLaggardIncumbentsLoseSeatsAtTheRise(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}
	src := os.Getenv("TSS_LAGGARD_SOURCE")
	if src == "" {
		t.Skip("TSS_LAGGARD_SOURCE not set (a copy of this tree that announces the previous consensus line until /tmp/upgraded exists)")
	}
	const floorEpoch = 6
	laggards := []int{6, 7}
	cfg := tssTestConfig()
	cfg.Nodes = 7
	cfg.GenesisNode = 1
	cfg.OldCodeSourceDir = src
	cfg.OldCodeNodes = laggards
	cfg.OldCodeSysconfig = true
	cp := cfg.SysConfigOverrides.ConsensusParams
	cp.ConsensusVersionFloorMajor = 0
	cp.ConsensusVersionFloorConsensus = poaDevnetFloor()
	cp.ConsensusVersionFloorEpoch = floorEpoch
	d, ctx := startDevnetNoKey(t, cfg, 70*time.Minute)

	acct := func(n int) string { return fmt.Sprintf("%s%d", cfg.WitnessPrefix, n) }
	bare := func(ms []string) []string {
		out := make([]string, len(ms))
		for i, m := range ms {
			out[i] = strings.TrimPrefix(m, "hive:")
		}
		return out
	}
	hasAll := func(ms []string, ns []int) bool {
		for _, n := range ns {
			if !contains(bare(ms), acct(n)) {
				return false
			}
		}
		return true
	}
	hasAny := func(ms []string, ns []int) bool {
		for _, n := range ns {
			if contains(bare(ms), acct(n)) {
				return true
			}
		}
		return false
	}

	// Incumbents before the rise.
	if err := d.waitForElectionEpoch(ctx, 1, floorEpoch-1, 20*time.Minute); err != nil {
		t.Fatalf("never reached epoch %d: %v", floorEpoch-1, err)
	}
	pre, _ := d.GetElectionMembers(ctx, 1, floorEpoch-1)
	t.Logf("1. pre-rise committee epoch %d: %v", floorEpoch-1, bare(pre))
	if !hasAll(pre, laggards) {
		t.Fatalf("PRECONDITION FAILED: the laggards are not incumbents before the rise: %v", bare(pre))
	}

	// The rise.
	if err := d.waitForElectionEpoch(ctx, 1, floorEpoch, 10*time.Minute); err != nil {
		t.Fatalf("never reached epoch %d: %v", floorEpoch, err)
	}
	rise, _ := d.GetElectionMembers(ctx, 1, floorEpoch)
	t.Logf("2. rising committee epoch %d: %v", floorEpoch, bare(rise))
	t.Logf("2. seats: %s", pfSeatFingerprint(pfMustSeats(t, d, ctx, 1)))

	// The laggards upgrade one epoch late.
	for _, n := range laggards {
		if out, err := exec.CommandContext(ctx, "docker", "exec", d.containerName(n), "sh", "-c", "touch /tmp/upgraded").CombinedOutput(); err != nil {
			t.Fatalf("upgrade magi-%d: %v %s", n, err, out)
		}
		if err := d.StopNode(ctx, n); err != nil {
			t.Fatalf("stop magi-%d: %v", n, err)
		}
		if err := d.StartNode(ctx, n); err != nil {
			t.Fatalf("start magi-%d: %v", n, err)
		}
	}
	// Precondition: the upgrade is visible on chain (their latest witness record
	// announces the floor line). Without it the run proves nothing.
	client, err := d.mongoClient(ctx)
	if err != nil {
		t.Fatalf("mongo: %v", err)
	}
	defer client.Disconnect(ctx)
	for _, n := range laggards {
		deadline := time.Now().Add(10 * time.Minute)
		for {
			var w struct {
				Height          uint64 `bson:"height"`
				ProtocolVersion uint64 `bson:"protocol_version"`
			}
			err := client.Database(d.nodeDbName(1)).Collection("witnesses").FindOne(ctx,
				bson.M{"account": acct(n)}, options.FindOne().SetSort(bson.M{"height": -1})).Decode(&w)
			if err == nil && w.ProtocolVersion >= poaDevnetFloor() {
				t.Logf("3. %s now announces consensus %d (record at height %d)", acct(n), w.ProtocolVersion, w.Height)
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("PRECONDITION FAILED: %s never announced consensus >= %d after the upgrade (last: %d, err %v)", acct(n), poaDevnetFloor(), w.ProtocolVersion, err)
			}
			time.Sleep(5 * time.Second)
		}
	}
	upgradedAt, _ := latestElection(t, d, ctx, 1)
	t.Logf("3. magi-6 and magi-7 upgraded and restarted; on-chain upgrade seen during epoch %d", upgradedAt)

	// Several elections later.
	const later = 4
	if err := d.waitForElectionEpoch(ctx, 1, upgradedAt+later, 20*time.Minute); err != nil {
		t.Fatalf("never reached epoch %d: %v", upgradedAt+later, err)
	}
	backIn := false
	for ep := upgradedAt + 1; ep <= upgradedAt+later; ep++ {
		m, _ := d.GetElectionMembers(ctx, 1, ep)
		t.Logf("4. committee epoch %d: %v", ep, bare(m))
		if hasAny(m, laggards) {
			backIn = true
		}
	}
	seats := pfMustSeats(t, d, ctx, 1)
	seated := false
	for _, s := range seats {
		for _, n := range laggards {
			if s.Account == acct(n) {
				seated = true
			}
		}
	}
	t.Logf("4. seats after %d elections: %s", later, pfSeatFingerprint(seats))
	for _, n := range laggards {
		t.Logf("4. magi-%d gate log: %s", n, lastLine(func() string {
			out, _ := exec.CommandContext(ctx, "bash", "-c", fmt.Sprintf("docker logs %s 2>&1 | grep -i -E 'seat gate|not a seat|unseated|excluded' | tail -3", d.containerName(1))).CombinedOutput()
			return strings.TrimSpace(string(out))
		}()))
		break
	}

	if !hasAny(rise, laggards) && !seated && !backIn {
		t.Errorf("LAGGARD-SEATS: incumbents %v that upgraded one epoch after the rise hold no seat and were in no committee for %d elections; they can only return by admission vote", []string{acct(6), acct(7)}, later)
	} else {
		t.Logf("laggards: in rising committee=%v seated=%v backIn=%v", hasAny(rise, laggards), seated, backIn)
	}
}
