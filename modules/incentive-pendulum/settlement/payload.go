package settlement

import "sort"

// RewardReductionEntry is one row of the per-epoch reward-reduction list.
// `Bps` is the consolidated post-forgiveness, post-cap value applied to that
// account's effective bond for the distribution math. The principal
// HIVE_CONSENSUS bond is NOT debited.
//
// Per-tick evidence breakdown (block_production / attestation / tss_*) is
// intentionally not on this struct: the on-chain settlement record carries
// only the consolidated number to keep the block payload small. Per-evidence
// detail is recoverable by re-running rewards.AggregateTick over the L2
// evidence in the snapshot range — see ComputeReductionsForEpoch.
// TODO(post-mvp): consider surfacing per-evidence breakdown if explorer demand
// justifies the extra payload bytes.
type RewardReductionEntry struct {
	Account string `json:"account" refmt:"account"`
	Bps     int    `json:"bps" refmt:"bps"`
}

// DistributionEntry is one row of the per-account HBD distribution list paid
// out from the `pendulum:nodes` bucket at settlement time.
type DistributionEntry struct {
	Account string `json:"account" refmt:"account"`
	HBDAmt  int64  `json:"hbd_amount" refmt:"hbd_amount"`
}

// SettlementRecord is the L2 op payload carried as a BlockTypePendulumSettlement
// entry inside a VSC block. The closing committee composes it deterministically
// from chain state (committee bonds + on-chain L2 evidence + ledger bucket
// balance) and signs it via the normal VSC block BLS aggregation. The state
// engine applies it once on every node when it processes the carrying block.
//
// All sub-arrays are sorted by Account on construction so CBOR/JSON encoding
// is byte-stable across nodes.
type SettlementRecord struct {
	// Epoch is the closing epoch — the one whose service the rewards pay for.
	Epoch uint64 `json:"epoch" refmt:"epoch"`
	// PrevEpoch is the epoch settled in the previous record. Used as a chain
	// continuity check; the state engine's apply path requires
	// PrevEpoch == latestSettledEpoch.
	PrevEpoch uint64 `json:"prev_epoch" refmt:"prev_epoch"`

	// Block-height bounds of the L2 evidence window the reductions cover.
	// SnapshotRangeFrom is exclusive; SnapshotRangeTo is inclusive and
	// equals the slot height the record was composed at.
	SnapshotRangeFrom uint64 `json:"snapshot_range_from" refmt:"snapshot_range_from"`
	SnapshotRangeTo   uint64 `json:"snapshot_range_to" refmt:"snapshot_range_to"`

	// BucketBalanceHBD is the pendulum:nodes:hbd ledger balance the record was
	// composed against. TotalDistributedHBD + ResidualHBD == BucketBalanceHBD.
	BucketBalanceHBD    int64 `json:"bucket_balance_hbd" refmt:"bucket_balance_hbd"`
	TotalDistributedHBD int64 `json:"total_distributed_hbd" refmt:"total_distributed_hbd"`
	ResidualHBD         int64 `json:"residual_hbd" refmt:"residual_hbd"`

	// RewardReductions is the per-witness consolidated bps applied to bonds
	// before pro-rata distribution. Sorted lexicographically by Account.
	RewardReductions []RewardReductionEntry `json:"reward_reductions" refmt:"reward_reductions"`

	// Distributions is the per-account HBD payout. Sorted lexicographically.
	Distributions []DistributionEntry `json:"distributions" refmt:"distributions"`
}

// BuildSettlementRecord assembles a SettlementRecord with deterministic
// sub-array ordering so two honest nodes produce byte-equal payloads. The
// caller is responsible for ensuring the inputs themselves are deterministic
// (e.g., reductions derived from on-chain L2 evidence, distributions computed
// pro-rata from on-chain bonds).
func BuildSettlementRecord(
	epoch, prevEpoch uint64,
	snapshotRangeFrom, snapshotRangeTo uint64,
	bucketBalanceHBD, totalDistributedHBD, residualHBD int64,
	reductions []RewardReductionEntry,
	dists []DistributionEntry,
) SettlementRecord {
	out := SettlementRecord{
		Epoch:               epoch,
		PrevEpoch:           prevEpoch,
		SnapshotRangeFrom:   snapshotRangeFrom,
		SnapshotRangeTo:     snapshotRangeTo,
		BucketBalanceHBD:    bucketBalanceHBD,
		TotalDistributedHBD: totalDistributedHBD,
		ResidualHBD:         residualHBD,
		RewardReductions:    append([]RewardReductionEntry(nil), reductions...),
		Distributions:       append([]DistributionEntry(nil), dists...),
	}
	sort.Slice(out.RewardReductions, func(i, j int) bool {
		return out.RewardReductions[i].Account < out.RewardReductions[j].Account
	})
	sort.Slice(out.Distributions, func(i, j int) bool {
		return out.Distributions[i].Account < out.Distributions[j].Account
	})
	return out
}
