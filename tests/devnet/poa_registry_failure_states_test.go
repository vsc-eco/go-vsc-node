package devnet

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"vsc-node/modules/common/params"

	"go.mongodb.org/mongo-driver/bson"
)

// wipeRegistry deletes a single node's poa_seats collection, simulating the
// "this node LOST its registry" case the bootstrap guard is written to detect.
func wipeRegistry(t *testing.T, d *Devnet, ctx context.Context, node int) int64 {
	t.Helper()
	client, err := d.mongoClient(ctx)
	if err != nil {
		t.Fatalf("mongo: %v", err)
	}
	defer client.Disconnect(ctx)
	res, err := client.Database(d.nodeDbName(node)).Collection("poa_seats").DeleteMany(ctx, bson.M{})
	if err != nil {
		t.Fatalf("wiping poa_seats on magi-%d: %v", node, err)
	}
	return res.DeletedCount
}

// TestPoaLostRegistryFailsClosed is scenario D16.
//
// poa_seats is NOT part of any merklized state and the reindex trigger keys off a
// hive_blocks marker, so the collection can be dropped or restored independently
// of chain history. The code calls the consequence out explicitly
// (poa_seats.go:100-127): an empty registry has two very different causes, and
// conflating them means "a node that loses its registry silently re-seeds from
// whatever the CURRENT committee happens to be, and then disagrees with every peer
// that still holds the true one — a divergence with no checkpoint or repair path
// anywhere to catch it."
//
// So the required behaviour on a LOST registry is: refuse to re-seed, stay inert,
// and say so loudly. This test destroys one node's registry AFTER activation and
// requires exactly that. A node that silently re-bootstraps here is the worst
// outcome POA can produce: a permanent, undetectable split over who may be elected.
func TestPoaLostRegistryFailsClosed(t *testing.T) {
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

	d, ctx := startDevnetNoKey(t, cfg, 50*time.Minute)

	for n := 1; n <= cfg.Nodes; n++ {
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
	reference, err := d.poaSeats(ctx, 1)
	if err != nil || len(reference) == 0 {
		t.Fatalf("PRECONDITION FAILED: no reference registry (%v)", err)
	}
	refFp := seatFingerprint(reference)
	t.Logf("reference registry (%d seats): %s", len(reference), refFp)

	victim := cfg.Nodes
	if err := d.StopNode(ctx, victim); err != nil {
		t.Fatalf("stopping magi-%d: %v", victim, err)
	}
	deleted := wipeRegistry(t, d, ctx, victim)
	if deleted == 0 {
		t.Fatalf("PREMISE FAILED: magi-%d had no seats to delete, so nothing was lost", victim)
	}
	t.Logf("destroyed magi-%d's registry (%d rows deleted) and restarting it", victim, deleted)
	if err := d.StartNode(ctx, victim); err != nil {
		t.Fatalf("restarting magi-%d: %v", victim, err)
	}

	// Let it catch up and process several elections. If it is going to re-seed
	// incorrectly, this is when it happens.
	time.Sleep(6 * time.Minute)

	after, err := d.poaSeats(ctx, victim)
	if err != nil {
		t.Fatalf("reading magi-%d registry: %v", victim, err)
	}
	afterFp := seatFingerprint(after)
	t.Logf("magi-%d registry after the loss (%d seats): %s", victim, len(after), afterFp)

	switch {
	case len(after) == 0:
		t.Logf("ACCEPTABLE (fail-closed): magi-%d refused to re-seed and stayed empty. The seat gate "+
			"is inert on this node and it does not fabricate a registry. Operator must restore or "+
			"full-reindex.", victim)
		if hit, lines := d.nodeLogContains(victim, "not the activation transition", 4000); hit {
			t.Logf("guard spoke as designed:\n%s", poaTrunc(lines, 700))
		} else {
			t.Errorf("magi-%d failed closed but emitted NO 'not the activation transition' message. "+
				"An operator would have a silently inert node and no way to know why — the guard's "+
				"own comment says the safe response is to leave it empty AND say so loudly.", victim)
		}
	case afterFp == refFp:
		t.Logf("ACCEPTABLE (recovered): magi-%d rebuilt the IDENTICAL registry, so no divergence.", victim)
	default:
		t.Errorf("SILENT REGISTRY DIVERGENCE — the worst outcome POA can produce. magi-%d re-seeded a "+
			"DIFFERENT registry after losing its own, so it now disagrees with its peers about who may "+
			"be elected, with no checkpoint or repair path to detect it.\n  peers:   %s\n  magi-%d: %s",
			victim, refFp, victim, afterFp)
	}

	// Whatever happened, the node must be alive and must not have panicked.
	name := d.projectName + "-magi-" + itoa(victim)
	out, _ := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", name).CombinedOutput()
	if string(out) == "" || len(out) < 4 {
		t.Errorf("could not inspect magi-%d", victim)
	}
	if hit, lines := d.nodeLogContains(victim, "panic", 4000); hit {
		t.Errorf("magi-%d PANICKED after losing its registry:\n%s", victim, poaTrunc(lines, 900))
	}
}

// TestPoaCrashDuringActivation is scenario D17.
//
// bootstrapPoaSeats fires at exactly ONE transition and explicitly refuses to
// retry (poa_seats.go:420-426). A node that dies while that transition is being
// processed is therefore the most direct route to a permanently missing or partial
// registry: it never sees the one moment it was allowed to seed.
//
// This SIGKILLs a node (docker kill, not a graceful stop) around the activation
// transition, restarts it, and requires it to converge on the identical registry.
func TestPoaCrashDuringActivation(t *testing.T) {
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

	d, ctx := startDevnetNoKey(t, cfg, 50*time.Minute)

	victim := cfg.Nodes
	name := d.projectName + "-magi-" + itoa(victim)

	// Wait for the activation election to exist, then SIGKILL immediately — the
	// transition is being processed right around here.
	nctx, cancel := context.WithTimeout(ctx, 12*time.Minute)
	if err := d.waitForElectionEpoch(nctx, 1, 1, 12*time.Minute); err != nil {
		cancel()
		t.Fatalf("magi-1 never ingested epoch 1: %v", err)
	}
	cancel()

	out, err := exec.Command("docker", "kill", name).CombinedOutput()
	if err != nil {
		t.Fatalf("SIGKILL magi-%d: %v (%s)", victim, err, out)
	}
	t.Logf("SIGKILLed magi-%d at the activation transition (ungraceful, no shutdown hooks)", victim)

	time.Sleep(90 * time.Second)
	if err := d.StartNode(ctx, victim); err != nil {
		t.Fatalf("restarting magi-%d: %v", victim, err)
	}
	t.Logf("restarted magi-%d", victim)

	// Peers must reach flat weight and a seeded registry.
	for n := 1; n < victim; n++ {
		nctx, cancel := context.WithTimeout(ctx, 12*time.Minute)
		_ = d.waitForElectionEpoch(nctx, n, 2, 12*time.Minute)
		cancel()
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
	reference, err := d.poaSeats(ctx, 1)
	if err != nil || len(reference) == 0 {
		t.Fatalf("PRECONDITION FAILED: no reference registry (%v)", err)
	}
	refFp := seatFingerprint(reference)
	t.Logf("peer reference registry (%d seats): %s", len(reference), refFp)

	// Give the crashed node time to catch up.
	var gotFp string
	var got []poaSeatDoc
	deadline := time.Now().Add(12 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(30 * time.Second)
		got, err = d.poaSeats(ctx, victim)
		if err != nil {
			continue
		}
		gotFp = seatFingerprint(got)
		if gotFp == refFp {
			break
		}
	}

	if gotFp == refFp {
		t.Logf("CONFIRMED: magi-%d converged on the IDENTICAL registry (%d seats) after being killed "+
			"across the activation transition.", victim, len(got))
		return
	}
	if len(got) == 0 {
		t.Errorf("magi-%d has an EMPTY registry after crashing across the activation transition. "+
			"bootstrap fires exactly once and refuses to retry, so this node has permanently missed "+
			"its only chance to seed: its seat gate stays inert and it silently disagrees with peers "+
			"about who may be elected.", victim)
		return
	}
	t.Errorf("REGISTRY DIVERGENCE after a crash across the activation transition — magi-%d holds a "+
		"DIFFERENT registry from its peers.\n  peers:   %s\n  magi-%d: %s", victim, refFp, victim, gotFp)
}
