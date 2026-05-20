package devnet

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"
)

// -----------------------------------------------------------------
// Shared helpers for the pendulum E2E devnet test workstream
// (see docs/pendulum/devnet-test-scope.md).
//
// Calibration anchors:
//   - bls_pop_test.go (per-node Mongo reads, strict cross-node
//     equality, log-substring sweeps)
//   - tss_helpers_test.go (config builder + startDevnet pattern)
//
// What lives here:
//   - pendulumTestConfig: a Config sized for pendulum tests.
//   - readLatestSettlementMarker / readSettlementMarkerForEpoch:
//     direct Mongo reads against the per-node `pendulum_settlements`
//     collection (schema in modules/db/vsc/pendulum_settlements).
//   - waitForSettlementEpoch: poll until a settlement marker with
//     epoch >= minEpoch lands on the named node.
//   - readCommitteeBondAt: latest `hive_consensus` balance for an
//     account at or before a target block, matching what
//     incentive-pendulum/settlement.ReadCommitteeBonds resolves on
//     the live node.
//   - countHiveBlocksWithFeedPublishOps: counts hive_blocks ingested
//     on a node that carry a feed_publish op — the observable proxy
//     for "this node sees feeds landing" since the oracle's
//     FeedTracker is in-memory (no persistent oracle collection on
//     this branch after commit fe70b3f9/8b84dea1).
//
// What deliberately doesn't live here yet:
//   - A "settlement payload reader" that returns the full
//     DistributionEntry / RewardReductionApplied arrays. Those live
//     inside the VSC block that carried the BlockTypePendulumSettlement
//     op, and the GraphQL `SettlementRecord` projection (gated on
//     `election { settlement }`) is the cleanest read path. M3 will
//     add a GraphQL helper for that — for now M1 only exposes the
//     summary marker.
// -----------------------------------------------------------------

// pendulumTestConfig builds a devnet Config suited for tests that
// exercise the pendulum pipeline (feed publish → oracle window →
// settlement → distribution → reduction).
//
// Choices:
//   - 5 nodes: enough for a real committee with quorum, low enough
//     to keep boot time bounded.
//   - SkipFunding=false: pendulum needs HBD/HIVE flow to settle on,
//     so we need the post-setup funding round. (TSS tests skip
//     because keygen alone doesn't need contract fees.)
//   - Fast ElectionInterval: settlement is currently election-aligned
//     (BlockTypePendulumSettlement is emitted by the closing committee
//     proposer), so faster elections give us settlements within a
//     reasonable test runtime.
//   - LogLevel includes "pendulum=trace" so settlement-related
//     warnings show up in d.Logs sweeps.
//
// PendulumParams: there's no dedicated SysConfigOverrides knob on
// this branch for pendulum settlement cadence — settlement is driven
// by the election cycle in pendulum_settlement.go. If a future change
// adds one, plumb it through here.
func pendulumTestConfig() *Config {
	cfg := DefaultConfig()
	cfg.Nodes = 5
	cfg.LogLevel = "error,pendulum=trace,oracle=trace"
	cfg.SysConfigOverrides = &systemconfig.SysConfigOverrides{
		ConsensusParams: &params.ConsensusParams{
			ElectionInterval: 20, // ~60s per epoch on devnet
		},
	}
	return cfg
}

// settlementMarkerDoc mirrors the BSON shape of
// modules/db/vsc/pendulum_settlements/schema.go::SettlementMarker.
// Stored in the per-node `pendulum_settlements` collection
// (collectionName constant in settlements.go).
type settlementMarkerDoc struct {
	Epoch               uint64 `bson:"epoch"`
	BlockHeight         uint64 `bson:"block_height"`
	SnapshotRangeFrom   uint64 `bson:"snapshot_range_from"`
	SnapshotRangeTo     uint64 `bson:"snapshot_range_to"`
	TotalDistributedHBD int64  `bson:"total_distributed_hbd"`
	ResidualHBD         int64  `bson:"residual_hbd"`
}

// readLatestSettlementMarker returns the highest-epoch settlement
// marker recorded on the named node. Returns (nil, nil) — no error,
// no record — if the collection is empty (pre-pendulum elections,
// or first settlement still pending).
func readLatestSettlementMarker(ctx context.Context, d *Devnet, node int) (*settlementMarkerDoc, error) {
	client, err := d.mongoClient(ctx)
	if err != nil {
		return nil, err
	}
	defer client.Disconnect(ctx)

	coll := client.Database(d.nodeDbName(node)).Collection("pendulum_settlements")
	var rec settlementMarkerDoc
	err = coll.FindOne(ctx, bson.M{},
		options.FindOne().SetSort(bson.D{{Key: "epoch", Value: -1}})).Decode(&rec)
	if err != nil {
		// FindOne returns ErrNoDocuments when the collection is
		// empty; callers treat that as "no settlement yet."
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil
		}
		return nil, fmt.Errorf("magi-%d: reading latest settlement marker: %w", node, err)
	}
	return &rec, nil
}

// readSettlementMarkerForEpoch returns the settlement marker for the
// specific epoch on the named node, or (nil, nil) if no marker exists.
// Used by M3 to fetch the same epoch across all nodes for byte-stable
// comparison of fields the marker carries.
func readSettlementMarkerForEpoch(ctx context.Context, d *Devnet, node int, epoch uint64) (*settlementMarkerDoc, error) {
	client, err := d.mongoClient(ctx)
	if err != nil {
		return nil, err
	}
	defer client.Disconnect(ctx)

	coll := client.Database(d.nodeDbName(node)).Collection("pendulum_settlements")
	var rec settlementMarkerDoc
	if err := coll.FindOne(ctx, bson.M{"epoch": epoch}).Decode(&rec); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil
		}
		return nil, fmt.Errorf("magi-%d: reading settlement marker epoch %d: %w", node, epoch, err)
	}
	return &rec, nil
}

// waitForSettlementEpoch polls the named node until a settlement
// marker with Epoch >= minEpoch has been written, or timeout
// elapses. Returns the marker found.
func waitForSettlementEpoch(ctx context.Context, d *Devnet, node int, minEpoch uint64, timeout time.Duration) (*settlementMarkerDoc, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		rec, err := readLatestSettlementMarker(ctx, d, node)
		if err != nil {
			return nil, err
		}
		if rec != nil && rec.Epoch >= minEpoch {
			return rec, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return nil, fmt.Errorf("magi-%d: no settlement marker with epoch >= %d within %s", node, minEpoch, timeout)
}

// readCommitteeBondAt returns the HIVE_CONSENSUS balance on the
// named account at or before targetBlock — the same input
// incentive-pendulum/settlement.ReadCommitteeBonds reads via
// balanceDb.GetBalanceRecord. The account argument may be either
// bare ("alice") or hive-prefixed ("hive:alice"); we normalise to
// the storage form ("hive:alice") used by ledger_balances.
func readCommitteeBondAt(ctx context.Context, d *Devnet, node int, account string, targetBlock uint64) (int64, error) {
	client, err := d.mongoClient(ctx)
	if err != nil {
		return 0, err
	}
	defer client.Disconnect(ctx)

	hiveAcc := account
	if len(account) < 5 || account[:5] != "hive:" {
		hiveAcc = "hive:" + account
	}

	coll := client.Database(d.nodeDbName(node)).Collection("ledger_balances")
	var raw struct {
		Account        string `bson:"account"`
		BlockHeight    uint64 `bson:"block_height"`
		HIVE_CONSENSUS int64  `bson:"hive_consensus"`
	}
	err = coll.FindOne(ctx,
		bson.M{
			"account":      hiveAcc,
			"block_height": bson.M{"$lte": targetBlock},
		},
		options.FindOne().SetSort(bson.D{{Key: "block_height", Value: -1}}),
	).Decode(&raw)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			// No balance record at or before targetBlock = bond is 0,
			// matching how the live ReadCommitteeBonds treats a
			// member with no balance history yet.
			return 0, nil
		}
		return 0, fmt.Errorf("magi-%d: reading bond for %s at block %d: %w", node, hiveAcc, targetBlock, err)
	}
	return raw.HIVE_CONSENSUS, nil
}

// countHiveBlocksWithFeedPublishOps counts hive_blocks documents on
// the named node whose embedded ops carry a `feed_publish` op type
// with block_number in [fromBlock, toBlock]. Used by M2 as the
// observable proxy for "feeds are reaching this node" since the
// oracle FeedTracker is in-memory (no persistent oracle collection
// on this branch).
//
// The hive_blocks document layout (modules/db/vsc/hive_blocks) puts
// the block payload under `block`. Hive ops are stored as
// {operations: [{type, value: {...}}]} per Hive's standard JSON
// shape. The exact path may vary across pre-/post-HAF formats —
// this helper does a loose substring scan rather than a strict
// path lookup for that reason.
func countHiveBlocksWithFeedPublishOps(ctx context.Context, d *Devnet, node int, fromBlock, toBlock uint64) (int64, error) {
	client, err := d.mongoClient(ctx)
	if err != nil {
		return 0, err
	}
	defer client.Disconnect(ctx)

	coll := client.Database(d.nodeDbName(node)).Collection("hive_blocks")
	// $where is brittle but the hive_blocks schema mixes top-level
	// and nested fields across versions; a regex over the serialised
	// doc is the most robust cross-version count. The collection is
	// indexed by block number, so the range filter is the cheap part.
	count, err := coll.CountDocuments(ctx, bson.M{
		"$and": []bson.M{
			{"block.block_number": bson.M{"$gte": fromBlock, "$lte": toBlock}},
			{"block": bson.M{"$regex": "feed_publish"}},
		},
	})
	if err != nil {
		return 0, fmt.Errorf("magi-%d: counting feed_publish hive_blocks in [%d, %d]: %w", node, fromBlock, toBlock, err)
	}
	return count, nil
}

