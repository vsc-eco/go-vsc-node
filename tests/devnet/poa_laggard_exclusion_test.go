package devnet

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"vsc-node/modules/common/params"

	"go.mongodb.org/mongo-driver/bson"
)

// announcedVersion reads an account's announced consensus triple from a node's
// witnesses collection.
func (d *Devnet) announcedVersion(ctx context.Context, node int, account string) (uint64, uint64, bool) {
	client, err := d.mongoClient(ctx)
	if err != nil {
		return 0, 0, false
	}
	defer client.Disconnect(ctx)
	var doc struct {
		VersionMajor uint64 `bson:"version_major"`
		Protocol     uint64 `bson:"protocol_version"`
	}
	err = client.Database(d.nodeDbName(node)).Collection("witnesses").
		FindOne(ctx, bson.M{"account": account}).Decode(&doc)
	if err != nil {
		return 0, 0, false
	}
	return doc.VersionMajor, doc.Protocol, true
}

// TestPoaLaggardExcludedFromFoundingCohort is scenario D11.
//
// Two facts compose into something operators must understand BEFORE they pick an
// activation epoch:
//
//  1. the election proposer DELETES any witness announcing below the version
//     floor (election-proposer.go), and
//  2. bootstrapPoaSeats seeds the PERMANENT, APPEND-ONLY seat registry from
//     whatever that ratified election contained (poa_seats.go:392).
//
// So a witness that has not upgraded by the activation epoch is not merely
// skipped for one epoch — it is excluded from the FOUNDING COHORT, and
// afterwards needs a ceil(2/3) admit_vote to get in at all. On the live testnet
// that is 4 of 5 existing operators agreeing to admit it.
//
// The laggard is a real node built from origin/main, which announces
// currentConsensus=3 (verified) against a 0.7.0 floor — not a mock.
//
// Set POA_OLD_SOURCE to a go-vsc-node checkout that announces below 0.7.0.
func TestPoaLaggardExcludedFromFoundingCohort(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}
	oldSrc := os.Getenv("POA_OLD_SOURCE")
	if oldSrc == "" {
		t.Skip("POA_OLD_SOURCE not set (path to a checkout announcing < 0.7.0)")
	}

	cfg := tssTestConfig()
	if cfg.SysConfigOverrides.ConsensusParams == nil {
		cfg.SysConfigOverrides.ConsensusParams = &params.ConsensusParams{}
	}
	// ★ FloorEpoch MUST be non-zero.
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorMajor = 0
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorConsensus = 7
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorEpoch = 1

	laggard := cfg.Nodes // magi-5 runs the old binary
	cfg.OldCodeSourceDir = oldSrc
	cfg.OldCodeNodes = []int{laggard}

	d, ctx := startDevnetNoKey(t, cfg, 45*time.Minute)

	// Only the UPGRADED nodes will form a committee; poll one of them.
	for n := 1; n < laggard; n++ {
		nctx, cancel := context.WithTimeout(ctx, 12*time.Minute)
		err := d.waitForElectionEpoch(nctx, n, 2, 12*time.Minute)
		cancel()
		if err != nil {
			t.Fatalf("magi-%d never reached epoch 2: %v", n, err)
		}
	}

	elec, err := d.GetElectionGQL(ctx, 1, 2)
	if err != nil {
		t.Fatalf("reading election epoch 2: %v", err)
	}
	for i, w := range elec.Weights {
		if w != params.PoaSeatWeight {
			t.Fatalf("PRECONDITION FAILED: weight[%d]=%d want flat %d — POA INERT", i, w, params.PoaSeatWeight)
		}
	}

	laggardAcct := d.witnessAccount(laggard)

	// ★ ESTABLISH THE PREMISE BEFORE CONCLUDING ANYTHING FROM THE ABSENCE.
	// Without these two checks this test cannot distinguish "excluded by the
	// version floor" (what it claims to prove) from "the old-code image never
	// built, so the container is simply not there" — and it would report PASS for
	// the second. An absence only means something once the thing is known to exist.
	lagName := d.projectName + "-magi-" + itoa(laggard)
	out, ierr := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", lagName).CombinedOutput()
	if ierr != nil || strings.TrimSpace(string(out)) != "true" {
		t.Fatalf("PREMISE FAILED: laggard container %s is not running (state=%q err=%v). Its absence "+
			"from the committee would then prove nothing about the version filter.",
			lagName, strings.TrimSpace(string(out)), ierr)
	}
	major, proto, found := d.announcedVersion(ctx, 1, laggardAcct)
	if !found {
		t.Fatalf("PREMISE FAILED: no witness record for %s on magi-1. The laggard never announced, so "+
			"it was never a candidate and its exclusion is not attributable to the version floor.",
			laggardAcct)
	}
	if major != 0 || proto >= 7 {
		t.Fatalf("PREMISE FAILED: %s announces %d.%d, which is NOT below the 0.7.0 floor. The "+
			"old-code image is not actually old, so this test is not exercising the version filter.",
			laggardAcct, major, proto)
	}
	t.Logf("PREMISE OK: laggard %s is RUNNING and announces %d.%d (below the 0.7.0 floor)",
		laggardAcct, major, proto)

	t.Logf("committee at epoch 2: %v (laggard=%s runs the pre-0.7.0 binary)", elec.Members, laggardAcct)

	// ---- the laggard must be OUT of the committee ----
	for _, m := range elec.Members {
		if m == laggardAcct || m == "hive:"+laggardAcct {
			t.Errorf("laggard %s IS in the committee despite announcing below the floor — the "+
				"version filter did not apply", laggardAcct)
		}
	}
	if len(elec.Members) != cfg.Nodes-1 {
		t.Logf("NOTE: committee has %d members, expected %d (all but the laggard)",
			len(elec.Members), cfg.Nodes-1)
	}

	// ---- and OUT of the permanent founding registry ----
	seats, err := d.poaSeats(ctx, 1)
	if err != nil {
		t.Fatalf("reading poa_seats: %v", err)
	}
	if len(seats) == 0 {
		t.Fatal("PRECONDITION FAILED: registry empty while POA is active")
	}
	t.Logf("founding registry (%d seats): %s", len(seats), seatFingerprint(seats))
	for _, s := range seats {
		if s.Account == laggardAcct {
			t.Errorf("laggard %s was SEEDED INTO THE PERMANENT REGISTRY despite being filtered "+
				"from the committee — the founding cohort must contain only ratified members",
				laggardAcct)
		}
	}
	if len(seats) != len(elec.Members) {
		t.Errorf("registry has %d seats but the committee has %d members", len(seats), len(elec.Members))
	}

	// ---- the consequence operators need to hear ----
	total := uint64(len(seats))
	required := total - total/3
	t.Logf("CONSEQUENCE: %s is now permanently outside the founding cohort. Re-entry requires "+
		"%d of %d seats to vote it in via vsc.admit_vote. On the live testnet that is 4 of 5 "+
		"operators. Upgrading BEFORE the activation epoch is materially cheaper than after.",
		laggardAcct, required, total)
}
