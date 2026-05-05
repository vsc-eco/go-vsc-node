package settlement

import "testing"

func TestBuildAndValidatePayload(t *testing.T) {
	a := BuildSettlementPayload(
		12,
		11,
		[]RewardReductionEntry{{Account: "hive:b", Bps: 25}, {Account: "hive:a", Bps: 50}},
		[]DistributionEntry{{Account: "hive:z", HBDAmt: 1}, {Account: "hive:a", HBDAmt: 2}},
	)
	b := BuildSettlementPayload(
		12,
		11,
		[]RewardReductionEntry{{Account: "hive:a", Bps: 50}, {Account: "hive:b", Bps: 25}},
		[]DistributionEntry{{Account: "hive:a", HBDAmt: 2}, {Account: "hive:z", HBDAmt: 1}},
	)
	if err := ValidateSettlementPayloadDeterministic(a, b); err != nil {
		t.Fatalf("expected deterministic equality, got: %v", err)
	}
}

func TestValidatePayloadDetectsMismatch(t *testing.T) {
	a := BuildSettlementPayload(1, 0, nil, []DistributionEntry{{Account: "hive:a", HBDAmt: 10}})
	b := BuildSettlementPayload(1, 0, nil, []DistributionEntry{{Account: "hive:a", HBDAmt: 11}})
	if err := ValidateSettlementPayloadDeterministic(a, b); err == nil {
		t.Fatal("expected mismatch error")
	}
}
