package settlement

import (
	"fmt"

	"vsc-node/modules/incentive-pendulum/rewards"
)

// ComposeInputs is the deterministic, on-chain-only input bundle the block
// producer feeds to ComposeRecord. Every field must be a function of chain
// state at slotHeight; nothing here may read local-only data (snapshots,
// transient caches, wall-clock time) or the result will diverge across nodes.
type ComposeInputs struct {
	// Epoch is the closing epoch (== electionDb.GetElectionByHeight(slotHeight).Epoch).
	Epoch uint64
	// PrevEpoch is the epoch that was settled in the prior record (== latest
	// settled epoch before this composition). Used as a chain-continuity
	// check at apply time.
	PrevEpoch uint64
	// EpochStartBh is the block_height where the closing election was
	// anchored. Used as the exclusive low end of the snapshot range.
	EpochStartBh uint64
	// SlotHeight is the inclusive high end of the snapshot range, i.e., the
	// block this settlement record is being produced in.
	SlotHeight uint64
	// CommitteeBonds is per-account HIVE_CONSENSUS at SlotHeight, keyed in
	// "hive:account" form. Read by ReadCommitteeBonds at the producer.
	CommitteeBonds map[string]int64
	// BucketBalanceHBD is the pendulum:nodes:hbd ledger balance at SlotHeight.
	BucketBalanceHBD int64
	// ReductionsByAccount is the per-witness consolidated reduction bps
	// computed over the closed epoch, keyed in "hive:account" form. Produced
	// by rewards.ComputeReductionsForEpoch from on-chain L2 evidence.
	ReductionsByAccount map[string]int
	// ShareDelegations holds, for each committee node running
	// delegationmode.Share at SlotHeight, that node's stake edges:
	// node -> (delegator -> net HIVE staked), both in "hive:account" form.
	// Nodes absent here keep their reward at the node account (Custom /
	// Deactivated / legacy). Consensus 0.3.0+; nil pre-activation, which makes
	// ComposeRecord byte-identical to the node-level result. Must be derived
	// deterministically (StateEngine.PendulumShareDelegations at SlotHeight) on
	// both the producer and every re-deriving node.
	ShareDelegations map[string]map[string]int64
}

// ComposeRecord builds the deterministic SettlementRecord for the closing
// epoch. Pure function: no I/O, no side effects, no state mutation. Two honest
// nodes with the same ComposeInputs produce byte-equal output.
//
// An empty-activity epoch (BucketBalanceHBD == 0) returns a marker-only
// record with no distributions and no reductions. A fully-reduced epoch
// (totalEffectiveBond == 0 after applying ReductionsByAccount) likewise
// returns a marker with the full bucket assigned to ResidualHBD and the
// reductions preserved on-chain. The next epoch's election gates on
// `latestSettled >= prior_epoch`, so a stalling error in this layer freezes
// the chain — both marker paths exist specifically to keep liveness when
// no distribution is owed.
//
// Returns nil + error only on inputs that prevent any meaningful record
// (zero epoch, slot at or before the epoch start, negative bucket, or
// distribution math that overflows the bucket).
func ComposeRecord(in ComposeInputs) (*SettlementRecord, error) {
	if in.Epoch == 0 {
		return nil, fmt.Errorf("epoch must be > 0")
	}
	if in.SlotHeight <= in.EpochStartBh {
		return nil, fmt.Errorf("slotHeight (%d) must exceed epochStartBh (%d)", in.SlotHeight, in.EpochStartBh)
	}
	if in.BucketBalanceHBD < 0 {
		return nil, fmt.Errorf("negative bucket balance %d", in.BucketBalanceHBD)
	}

	// Empty-activity epoch: emit a marker-only record so the chain can
	// advance to the next election. Bonds and reductions are not required —
	// nothing is being distributed.
	if in.BucketBalanceHBD == 0 {
		rec := BuildSettlementRecord(
			in.Epoch,
			in.PrevEpoch,
			in.EpochStartBh,
			in.SlotHeight,
			0, 0, 0,
			nil,
			nil,
		)
		return &rec, nil
	}

	if len(in.CommitteeBonds) == 0 {
		return nil, fmt.Errorf("committee bonds map is empty")
	}

	effectiveBonds, applied := ApplyRewardReductionsToBonds(in.CommitteeBonds, in.ReductionsByAccount)

	totalEffectiveBond := int64(0)
	for _, b := range effectiveBonds {
		totalEffectiveBond += b
	}
	if totalEffectiveBond <= 0 {
		// #52: universal 100% reduction across the committee (every member at
		// PerEpochCapBps) collapses every effective bond to 0. Erroring here
		// aborted election production deterministically — a universal stall
		// reachable purely from on-chain evidence, with no swap path to clear
		// because the bucket is non-zero. Emit a marker-only record instead:
		// the bucket rolls into the next epoch via residual, the reductions
		// remain on the audit trail, and the chain advances.
		reductionPayload := make([]RewardReductionEntry, 0, len(applied))
		for _, r := range applied {
			reductionPayload = append(reductionPayload, RewardReductionEntry{
				Account: r.Account,
				Bps:     r.Bps,
			})
		}
		rec := BuildSettlementRecord(
			in.Epoch,
			in.PrevEpoch,
			in.EpochStartBh,
			in.SlotHeight,
			in.BucketBalanceHBD,
			0,
			in.BucketBalanceHBD,
			reductionPayload,
			nil,
		)
		return &rec, nil
	}

	// All HBD in the bucket goes to nodes — LP retention happened at swap
	// time per W3, the bucket only holds the node-runner share.
	nodeShare := in.BucketBalanceHBD

	dists := ComputeNodeDistributions(nodeShare, effectiveBonds)

	// Consensus 0.3.0: expand each share-mode node's slice into per-delegator
	// payouts (pro-rata by stake); Custom / Deactivated / legacy nodes pass
	// through unchanged. Each node's total is preserved exactly (rounding
	// remainder → operator), so totalDistributed and residual are identical to
	// the node-level result and the conservation invariant is unchanged. With
	// no share nodes (ShareDelegations nil/empty) this is a no-op and the record
	// is byte-identical to pre-0.3.0.
	dists = ExpandShareDistributions(dists, in.ShareDelegations)

	distPayload := make([]DistributionEntry, 0, len(dists))
	totalDistributed := int64(0)
	for _, d := range dists {
		if d.Amount <= 0 {
			continue
		}
		distPayload = append(distPayload, DistributionEntry{
			Account: d.Account,
			HBDAmt:  d.Amount,
		})
		totalDistributed += d.Amount
	}

	reductionPayload := make([]RewardReductionEntry, 0, len(applied))
	for _, r := range applied {
		reductionPayload = append(reductionPayload, RewardReductionEntry{
			Account: r.Account,
			Bps:     r.Bps,
		})
	}

	// ComputeNodeDistributions floors every per-node share, so totalDistributed
	// is always <= BucketBalanceHBD; residual is the rounding remainder, which
	// the apply path leaves in the pendulum:nodes bucket to roll into the next
	// epoch. The < 0 guard is purely defensive — floored shares cannot overspend
	// the bucket.
	residual := in.BucketBalanceHBD - totalDistributed
	if residual < 0 {
		return nil, fmt.Errorf("distribution sum %d exceeds bucket %d", totalDistributed, in.BucketBalanceHBD)
	}

	rec := BuildSettlementRecord(
		in.Epoch,
		in.PrevEpoch,
		in.EpochStartBh,
		in.SlotHeight,
		in.BucketBalanceHBD,
		totalDistributed,
		residual,
		reductionPayload,
		distPayload,
	)
	return &rec, nil
}

// SortedAccountsFromMap re-exports the rewards helper so callers in this
// package don't have to import rewards/ for the one helper.
func SortedAccountsFromMap(m map[string]int) []string {
	return rewards.SortedAccountsFromMap(m)
}
