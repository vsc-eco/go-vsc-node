package settlement

import (
	"fmt"
	"sort"
)

// RewardReductionEntry is one row of the per-epoch reward-reduction list.
// Bps is the post-forgiveness post-cap value applied to that account's
// effective bond for the distribution math. Principal HIVE_CONSENSUS is
// NOT debited.
type RewardReductionEntry struct {
	Account string `json:"account"`
	Bps     int    `json:"bps"`
}

type DistributionEntry struct {
	Account string `json:"account"`
	HBDAmt  int64  `json:"hbd_amount"`
}

// SettlementPayload is the wire format for vsc.pendulum_settlement.
// All sub-arrays are sorted on construction for byte-deterministic
// JSON encoding across nodes.
type SettlementPayload struct {
	Epoch            uint64                 `json:"epoch"`
	PrevEpoch        uint64                 `json:"prev_epoch"`
	RewardReductions []RewardReductionEntry `json:"reward_reductions"`
	Dists            []DistributionEntry    `json:"distributions"`
}

func BuildSettlementPayload(
	epoch uint64,
	prevEpoch uint64,
	reductions []RewardReductionEntry,
	dists []DistributionEntry,
) SettlementPayload {
	out := SettlementPayload{
		Epoch:            epoch,
		PrevEpoch:        prevEpoch,
		RewardReductions: append([]RewardReductionEntry(nil), reductions...),
		Dists:            append([]DistributionEntry(nil), dists...),
	}
	sort.Slice(out.RewardReductions, func(i, j int) bool {
		return out.RewardReductions[i].Account < out.RewardReductions[j].Account
	})
	sort.Slice(out.Dists, func(i, j int) bool { return out.Dists[i].Account < out.Dists[j].Account })
	return out
}

func ValidateSettlementPayloadDeterministic(expected SettlementPayload, got SettlementPayload) error {
	if expected.Epoch != got.Epoch || expected.PrevEpoch != got.PrevEpoch {
		return fmt.Errorf("epoch mismatch")
	}
	if len(expected.RewardReductions) != len(got.RewardReductions) || len(expected.Dists) != len(got.Dists) {
		return fmt.Errorf("payload cardinality mismatch")
	}
	for i := range expected.RewardReductions {
		if expected.RewardReductions[i] != got.RewardReductions[i] {
			return fmt.Errorf("reward reduction mismatch at %d", i)
		}
	}
	for i := range expected.Dists {
		if expected.Dists[i] != got.Dists[i] {
			return fmt.Errorf("distribution mismatch at %d", i)
		}
	}
	return nil
}
