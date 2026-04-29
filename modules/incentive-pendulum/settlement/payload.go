package settlement

import (
	"fmt"
	"sort"
)

type SlashEntry struct {
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
	Epoch     uint64              `json:"epoch"`
	PrevEpoch uint64              `json:"prev_epoch"`
	Slashes   []SlashEntry        `json:"slashes"`
	Dists     []DistributionEntry `json:"distributions"`
}

func BuildSettlementPayload(
	epoch uint64,
	prevEpoch uint64,
	slashes []SlashEntry,
	dists []DistributionEntry,
) SettlementPayload {
	out := SettlementPayload{
		Epoch:     epoch,
		PrevEpoch: prevEpoch,
		Slashes:   append([]SlashEntry(nil), slashes...),
		Dists:     append([]DistributionEntry(nil), dists...),
	}
	sort.Slice(out.Slashes, func(i, j int) bool { return out.Slashes[i].Account < out.Slashes[j].Account })
	sort.Slice(out.Dists, func(i, j int) bool { return out.Dists[i].Account < out.Dists[j].Account })
	return out
}

func ValidateSettlementPayloadDeterministic(expected SettlementPayload, got SettlementPayload) error {
	if expected.Epoch != got.Epoch || expected.PrevEpoch != got.PrevEpoch {
		return fmt.Errorf("epoch mismatch")
	}
	if len(expected.Slashes) != len(got.Slashes) || len(expected.Dists) != len(got.Dists) {
		return fmt.Errorf("payload cardinality mismatch")
	}
	for i := range expected.Slashes {
		if expected.Slashes[i] != got.Slashes[i] {
			return fmt.Errorf("slash mismatch at %d", i)
		}
	}
	for i := range expected.Dists {
		if expected.Dists[i] != got.Dists[i] {
			return fmt.Errorf("distribution mismatch at %d", i)
		}
	}
	return nil
}
