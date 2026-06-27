package db

import (
	"context"
	"fmt"
	"slices"
	"vsc-node/lib/vsclog"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

var log = vsclog.Module("db")

var REINDEX_ID = 18

var IMMUTABLE_COLLECTIONS = []string{
	"hive_blocks",
}

// VersionReindex configures the consensus-version-lag full-reindex trigger.
//
// A node that ran a binary BELOW the chain-active consensus version while it
// processed blocks diverges locally (it applies stale rules to blocks the network
// adopted a newer version for — exactly the lagging-behind-the-80%-adoption-window
// case). Upgrading the binary does NOT heal the already-written rows; the history
// must be replayed under the correct rules. As a temporary measure (until a partial
// reindex exists) this triggers a FULL reindex (drop derived state, replay from
// genesis) when the previous run's recorded version could not handle the chain-active
// version at the head and THIS binary can.
//
// Nil disables the trigger. The chain-active lookup is injected as a callback so this
// generic db package stays free of election/consensus-version imports.
type VersionReindex struct {
	RunningMajor     uint64
	RunningConsensus uint64
	// ChainActiveAt returns the chain-active coordinated version (major.consensus) at
	// a block height, sourced from the on-chain election. ok=false when unknown
	// (no election below the height / pre-genesis) — the trigger is then skipped.
	ChainActiveAt func(blockHeight uint64) (major, consensus uint64, ok bool)
}

type DbReindex struct {
	*DbInstance
	force    bool
	verCheck *VersionReindex
}

func (dbr *DbReindex) Init() error {
	ctx := context.Background()
	col := dbr.Collection("hive_blocks")
	result := SearchResult{}
	_ = col.FindOne(ctx, bson.M{"type": "metadata"}).Decode(&result)

	var indexId uint64
	if result.ReindexId != nil {
		indexId = *result.ReindexId
	}

	shouldReindex := indexId != uint64(REINDEX_ID) || dbr.force
	reason := ""
	switch {
	case dbr.force:
		reason = "force flag"
	case indexId != uint64(REINDEX_ID):
		reason = fmt.Sprintf("reindex_id %d != %d", indexId, REINDEX_ID)
	}

	// Consensus-version-lag trigger. Skipped on the first boot that records a version
	// (ProcessedUnderConsensus nil) — we assume the existing DB is consistent with the
	// current binary rather than forcing an unnecessary genesis replay; the trigger
	// then arms for any FUTURE lag.
	if dbr.verCheck != nil && dbr.verCheck.ChainActiveAt != nil && result.ProcessedUnderConsensus != nil {
		var lastBlock uint64
		if result.LastProcessedBlock != nil {
			lastBlock = *result.LastProcessedBlock
		}
		chMajor, chCons, ok := dbr.verCheck.ChainActiveAt(lastBlock)
		if ok {
			procMajor := uint64(0)
			if result.ProcessedUnderMajor != nil {
				procMajor = *result.ProcessedUnderMajor
			}
			procCons := *result.ProcessedUnderConsensus
			if versionLagNeedsReindex(dbr.verCheck.RunningMajor, dbr.verCheck.RunningConsensus, chMajor, chCons, procMajor, procCons) {
				shouldReindex = true
				reason = fmt.Sprintf("consensus-version lag: last processed under %d.%d but chain-active was %d.%d at block %d (this binary %d.%d)",
					procMajor, procCons, chMajor, chCons, lastBlock, dbr.verCheck.RunningMajor, dbr.verCheck.RunningConsensus)
			}
		}
	}

	if shouldReindex {
		log.Info("reindexing database (full replay from genesis)", "reason", reason, "from", indexId, "to", REINDEX_ID)
		cols, _ := dbr.ListCollectionNames(ctx, bson.M{})
		for _, name := range cols {
			if !slices.Contains(IMMUTABLE_COLLECTIONS, name) {
				dbr.Collection(name).Drop(ctx)
			}
		}
		col.FindOneAndUpdate(ctx, bson.M{
			"type": "metadata",
		}, bson.M{
			"$set": bson.M{
				"reindex_id":           REINDEX_ID,
				"last_processed_block": 0,
			},
		}, options.FindOneAndUpdate().SetUpsert(true))
	}

	// Record THIS run's binary version so the next start can detect a future version
	// lag. Written whether or not we reindexed: after a wipe it tags the upcoming
	// genesis replay; on a normal restart it tags forward processing.
	if dbr.verCheck != nil {
		col.FindOneAndUpdate(ctx, bson.M{
			"type": "metadata",
		}, bson.M{
			"$set": bson.M{
				"processed_under_major":     dbr.verCheck.RunningMajor,
				"processed_under_consensus": dbr.verCheck.RunningConsensus,
			},
		}, options.FindOneAndUpdate().SetUpsert(true))
	}
	return nil
}

// versionLagNeedsReindex reports whether a full reindex is required because the
// version that last processed up to the head (proc*) could NOT handle the chain-active
// version there (ch*) — the node lagged behind a version adoption and diverged —
// while THIS binary (running*) can. Componentwise on the coordinated major.consensus
// (same MeetsConsensusMin semantics the election floor uses).
func versionLagNeedsReindex(runningMajor, runningCons, chMajor, chCons, procMajor, procCons uint64) bool {
	runningMeets := runningMajor >= chMajor && runningCons >= chCons
	processedMeets := procMajor >= chMajor && procCons >= chCons
	return runningMeets && !processedMeets
}

func NewReindex(db *DbInstance, force bool, verCheck *VersionReindex) *DbReindex {
	return &DbReindex{db, force, verCheck}
}

type SearchResult struct {
	ReindexId               *uint64 `json:"reindex_id,omitempty" bson:"reindex_id,omitempty"`
	LastProcessedBlock      *uint64 `json:"last_processed_block,omitempty" bson:"last_processed_block,omitempty"`
	ProcessedUnderMajor     *uint64 `json:"processed_under_major,omitempty" bson:"processed_under_major,omitempty"`
	ProcessedUnderConsensus *uint64 `json:"processed_under_consensus,omitempty" bson:"processed_under_consensus,omitempty"`
}
