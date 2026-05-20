package devnet

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestPendulumSettlementHappyPath is M3 of the pendulum E2E
// workstream (docs/pendulum/devnet-test-scope.md). The flagship test.
//
// Proves end-to-end determinism of the pendulum settlement path:
//
//  1. A settlement marker eventually lands in every node's
//     `pendulum_settlements` collection.
//  2. The marker's canonical fields (epoch, block_height,
//     snapshot_range_from, snapshot_range_to, total_distributed_hbd,
//     residual_hbd) are byte-identical across every node for the
//     same epoch — i.e. settlement consensus actually converged.
//  3. TotalDistributedHBD > 0 — at least one DistributionEntry was
//     produced; the settlement didn't degenerate to a no-op.
//  4. Snapshot range is well-formed (from <= to, both > 0).
//
// What's deferred to a follow-up (call it M3.5):
//
//   - Reading the full DistributionEntry / RewardReductionApplied
//     arrays per node and asserting THEY also match byte-for-byte.
//     Those live in the VSC block carrying the
//     BlockTypePendulumSettlement op, not in pendulum_settlements
//     (which is summary-only). The cleanest read path is the
//     GraphQL `election { settlement }` projection
//     (modules/gql/schema.graphql defines SettlementRecord +
//     DistributionEntry). Adding a GraphQL settlement reader is
//     itself a piece of helper work; M3 ships without it so we
//     don't bundle two concerns in one milestone.
//   - Verifying per-member HBD balance deltas match the marker's
//     TotalDistributedHBD. This requires a separate HBD-balance
//     reader (M1 only added the HIVE_CONSENSUS bond reader).
//     Follow-up.
//
// Failure modes this catches:
//
//   - Non-deterministic settlement compose: marker.TotalDistributedHBD
//     or BlockHeight differs across nodes (consensus break — the
//     class of bug 8130b95e's deterministic-amount + namespace
//     fixes guard against).
//   - Settlement never lands: marker stays nil after the timeout
//     (pendulum scheduler is wedged, or oracle window never produced
//     usable inputs).
//   - Settlement produces zero distribution: marker.TotalDistributedHBD
//     == 0 (committee bond reader returned no bonds, fee inputs are
//     all zero, or stabilizer suppressed everything).
//
// Run with:
//
//	go test -v -run TestPendulumSettlementHappyPath -timeout 25m ./tests/devnet/
func TestPendulumSettlementHappyPath(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}

	cfg := pendulumTestConfig()
	d, ctx := startDevnetNoKey(t, cfg, 20*time.Minute)

	// Wait for every node to ingest at least one running election.
	// Settlement is election-aligned: a closing committee proposer
	// emits BlockTypePendulumSettlement when the epoch advances, so
	// no settlement can land before epoch >= 1.
	t.Log("waiting for first running election (epoch >= 1) on every node...")
	for n := 1; n <= cfg.Nodes; n++ {
		nodeCtx, cancel := context.WithTimeout(ctx, 8*time.Minute)
		if err := d.waitForElectionEpoch(nodeCtx, n, 1, 8*time.Minute); err != nil {
			cancel()
			t.Fatalf("magi-%d never ingested epoch >= 1: %v", n, err)
		}
		cancel()
	}

	// Pendulum settlement seeds at the genesis committee transition
	// then composes on subsequent committee transitions. Wait for a
	// settlement marker with Epoch >= 1 on magi-1 first (any node;
	// magi-1 is arbitrary). We give a generous timeout because the
	// first composition can lag the first election by a full epoch.
	t.Log("waiting for first settlement marker on magi-1 (epoch >= 1)...")
	first, err := waitForSettlementEpoch(ctx, d, 1, 1, 12*time.Minute)
	if err != nil {
		// Dump magi-1 logs to surface why settlement isn't landing.
		logs, _ := d.Logs(ctx, "magi-1")
		if logs != "" {
			t.Logf("magi-1 logs:\n%s", truncateLogs(logs, 40))
		}
		t.Fatalf("magi-1 never landed a settlement marker: %v", err)
	}
	targetEpoch := first.Epoch
	t.Logf("settlement landed on magi-1: epoch=%d block_height=%d total_distributed=%d residual=%d snapshot=[%d, %d]",
		first.Epoch, first.BlockHeight, first.TotalDistributedHBD, first.ResidualHBD,
		first.SnapshotRangeFrom, first.SnapshotRangeTo)

	// Wait for the same epoch's marker on every other node. Settlement
	// propagation across nodes is fast (it rides on VSC block ingest),
	// so 3 minutes is plenty.
	t.Logf("waiting for same epoch %d on remaining nodes...", targetEpoch)
	markers := make([]*settlementMarkerDoc, cfg.Nodes)
	markers[0] = first
	for n := 2; n <= cfg.Nodes; n++ {
		nodeCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		m, err := waitForSpecificSettlementEpoch(nodeCtx, d, n, targetEpoch, 3*time.Minute)
		cancel()
		if err != nil {
			t.Fatalf("magi-%d never landed settlement marker for epoch %d: %v", n, targetEpoch, err)
		}
		markers[n-1] = m
	}

	// Assertion 1: well-formed marker on magi-1 (sanity baseline).
	if first.TotalDistributedHBD <= 0 {
		t.Errorf("settlement marker on magi-1 has total_distributed_hbd=%d (expected > 0) — settlement produced no rewards",
			first.TotalDistributedHBD)
	}
	if first.SnapshotRangeTo == 0 || first.SnapshotRangeFrom > first.SnapshotRangeTo {
		t.Errorf("settlement marker on magi-1 has malformed snapshot range [%d, %d]",
			first.SnapshotRangeFrom, first.SnapshotRangeTo)
	}
	if first.BlockHeight == 0 {
		t.Errorf("settlement marker on magi-1 has block_height=0 (expected nonzero)")
	}

	// Assertion 2: every field of the marker is identical across all
	// nodes for the chosen epoch. Any divergence is a consensus break.
	for n := 2; n <= cfg.Nodes; n++ {
		m := markers[n-1]
		if m.Epoch != first.Epoch {
			t.Errorf("magi-%d epoch=%d differs from magi-1 epoch=%d", n, m.Epoch, first.Epoch)
		}
		if m.BlockHeight != first.BlockHeight {
			t.Errorf("magi-%d block_height=%d differs from magi-1 block_height=%d", n, m.BlockHeight, first.BlockHeight)
		}
		if m.SnapshotRangeFrom != first.SnapshotRangeFrom {
			t.Errorf("magi-%d snapshot_range_from=%d differs from magi-1 %d", n, m.SnapshotRangeFrom, first.SnapshotRangeFrom)
		}
		if m.SnapshotRangeTo != first.SnapshotRangeTo {
			t.Errorf("magi-%d snapshot_range_to=%d differs from magi-1 %d", n, m.SnapshotRangeTo, first.SnapshotRangeTo)
		}
		if m.TotalDistributedHBD != first.TotalDistributedHBD {
			t.Errorf("magi-%d total_distributed_hbd=%d differs from magi-1 %d (settlement amount diverged)",
				n, m.TotalDistributedHBD, first.TotalDistributedHBD)
		}
		if m.ResidualHBD != first.ResidualHBD {
			t.Errorf("magi-%d residual_hbd=%d differs from magi-1 %d", n, m.ResidualHBD, first.ResidualHBD)
		}
	}

	// Sanity sweep: settlement-validation warnings would indicate the
	// state engine rejected a settlement (consensus-affecting) even
	// though the marker eventually wrote. Surface them as errors.
	t.Run("no_settlement_validation_warnings", func(t *testing.T) {
		patterns := []string{
			"settlement validation failed",
			"pendulum compose drift",
			"settlement record mismatch",
			"applyPendulumSettlement: invalid",
		}
		for n := 1; n <= cfg.Nodes; n++ {
			logs, err := d.Logs(ctx, fmt.Sprintf("magi-%d", n))
			if err != nil {
				t.Logf("warning: could not read magi-%d logs: %v", n, err)
				continue
			}
			for _, pat := range patterns {
				if strings.Contains(logs, pat) {
					t.Errorf("magi-%d logs contain %q (settlement consensus warning)", n, pat)
				}
			}
		}
	})
}

// waitForSpecificSettlementEpoch is like waitForSettlementEpoch but
// waits for exactly `epoch`, not "epoch >= minEpoch". Used by M3 to
// fetch the same epoch's marker from every node.
func waitForSpecificSettlementEpoch(ctx context.Context, d *Devnet, node int, epoch uint64, timeout time.Duration) (*settlementMarkerDoc, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		m, err := readSettlementMarkerForEpoch(ctx, d, node, epoch)
		if err != nil {
			return nil, err
		}
		if m != nil {
			return m, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	return nil, fmt.Errorf("magi-%d: no settlement marker for epoch %d within %s", node, epoch, timeout)
}
