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
}

// ComposeRecord builds the deterministic SettlementRecord for the closing
// epoch. Pure function: no I/O, no side effects, no state mutation. Two honest
// nodes with the same ComposeInputs produce byte-equal output.
//
// Returns nil + error if the inputs are insufficient to settle (e.g., empty
// bucket, no committee bonds). The producer skips the block-tx in that case.
func ComposeRecord(in ComposeInputs) (*SettlementRecord, error) {
	if in.Epoch == 0 {
		return nil, fmt.Errorf("epoch must be > 0")
	}
	if in.SlotHeight <= in.EpochStartBh {
		return nil, fmt.Errorf("slotHeight (%d) must exceed epochStartBh (%d)", in.SlotHeight, in.EpochStartBh)
	}
	if in.BucketBalanceHBD <= 0 {
		// Empty bucket — no settlement to record. Caller treats as no-op.
		return nil, fmt.Errorf("bucket balance is 0; nothing to settle")
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
		return nil, fmt.Errorf("total effective bond after reductions is 0")
	}

	// All HBD in the bucket goes to nodes — LP retention happened at swap
	// time per W3, the bucket only holds the node-runner share.
	nodeShare := in.BucketBalanceHBD

	dists := ComputeNodeDistributions(nodeShare, effectiveBonds)

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

	residual := in.BucketBalanceHBD - totalDistributed
	if residual < 0 {
		// ComputeNodeDistributions assigns the floor residual to the largest
		// stake — the sum should equal nodeShare exactly. Defensive guard.
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

// PreCheckBucket returns the bucket-balance threshold below which composition
// should be skipped. Currently 1 base unit (any positive value); kept as a
// helper so the predicate is in one place if we want to add a minimum.
func PreCheckBucket(balanceHBD int64) bool {
	return balanceHBD > 0
}

// SortedAccountsFromMap re-exports the rewards helper so callers in this
// package don't have to import rewards/ for the one helper.
func SortedAccountsFromMap(m map[string]int) []string {
	return rewards.SortedAccountsFromMap(m)
}
