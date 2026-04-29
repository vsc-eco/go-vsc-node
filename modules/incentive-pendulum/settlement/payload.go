package settlement

import (
	"fmt"
	"sort"
)

type ConversionEntry struct {
	PoolID       string `json:"pool_id"`
	Asset        string `json:"asset"`
	NativeAmount int64  `json:"native_amount"`
	HBDOut       int64  `json:"hbd_amount_out"`
}

type SlashEntry struct {
	Account string `json:"account"`
	Bps     int    `json:"bps"`
}

type DistributionEntry struct {
	Account string `json:"account"`
	HBDAmt  int64  `json:"hbd_amount"`
}

type SettlementPayload struct {
	Epoch       uint64              `json:"epoch"`
	PrevEpoch   uint64              `json:"prev_epoch"`
	Conversions []ConversionEntry   `json:"conversions"`
	Slashes     []SlashEntry        `json:"slashes"`
	Dists       []DistributionEntry `json:"distributions"`
}

func BuildSettlementPayload(
	epoch uint64,
	prevEpoch uint64,
	conversions []ConversionEntry,
	slashes []SlashEntry,
	dists []DistributionEntry,
) SettlementPayload {
	out := SettlementPayload{
		Epoch:       epoch,
		PrevEpoch:   prevEpoch,
		Conversions: append([]ConversionEntry(nil), conversions...),
		Slashes:     append([]SlashEntry(nil), slashes...),
		Dists:       append([]DistributionEntry(nil), dists...),
	}
	sort.Slice(out.Conversions, func(i, j int) bool {
		if out.Conversions[i].PoolID == out.Conversions[j].PoolID {
			return out.Conversions[i].Asset < out.Conversions[j].Asset
		}
		return out.Conversions[i].PoolID < out.Conversions[j].PoolID
	})
	sort.Slice(out.Slashes, func(i, j int) bool { return out.Slashes[i].Account < out.Slashes[j].Account })
	sort.Slice(out.Dists, func(i, j int) bool { return out.Dists[i].Account < out.Dists[j].Account })
	return out
}

func ValidateSettlementPayloadDeterministic(expected SettlementPayload, got SettlementPayload) error {
	if expected.Epoch != got.Epoch || expected.PrevEpoch != got.PrevEpoch {
		return fmt.Errorf("epoch mismatch")
	}
	if len(expected.Conversions) != len(got.Conversions) || len(expected.Slashes) != len(got.Slashes) || len(expected.Dists) != len(got.Dists) {
		return fmt.Errorf("payload cardinality mismatch")
	}
	for i := range expected.Conversions {
		if expected.Conversions[i] != got.Conversions[i] {
			return fmt.Errorf("conversion mismatch at %d", i)
		}
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
