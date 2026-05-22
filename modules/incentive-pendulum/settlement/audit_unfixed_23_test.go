package settlement

import "testing"

// TestAuditUnfixed_23_NoPerNodeBondCapInDistribution — Pendulum audit MEDIUM
// #23 (no per-node bond cap / MaxStakeShareBps clamp in distribution).
//
// Precondition: ComputeNodeDistributions splits nodeShare strictly pro-rata
// by effective bond — see calculator.go:78-128. There is no upper bound on
// any single account's share; whoever holds the largest bond receives the
// largest cut, all the way up to ~100% of the bucket if they hold ~100% of
// the bond. The struct in calculator.go does not define MaxStakeShareBps and
// no caller clamps the result.
//
// The economic effect: a whale that controls ~95% of committee bond captures
// ~95% of every epoch's HBD bucket. This concentrates incentive-pendulum
// rewards toward whoever already has the most stake, with no protocol-level
// brake — the opposite of a "spread the reward across the committee" design.
//
// Differential — today the whale's distribution comes out at ~95% of the
// bucket (no cap). Post-fix the call should clamp to MaxStakeShareBps (e.g.
// 5000 bps = 50%) so no single node can take more than half the bucket
// regardless of how dominant their bond is.
func TestAuditUnfixed_23_NoPerNodeBondCapInDistribution(t *testing.T) {
	// One whale at 95% of total bond, 4 smaller members at 1.25% each.
	// Total bond = 95 + 4*1.25 = 100 (scaled to 100_000 for round arithmetic).
	bonds := map[string]int64{
		"hive:whale": 95_000,
		"hive:a":     1_250,
		"hive:b":     1_250,
		"hive:c":     1_250,
		"hive:d":     1_250,
	}
	const bucketHBD int64 = 1_000_000

	out := ComputeNodeDistributions(bucketHBD, bonds)
	if len(out) != 5 {
		t.Fatalf("audit #23: expected 5 distributions, got %d", len(out))
	}

	var whaleAmount int64
	for _, d := range out {
		if d.Account == "hive:whale" {
			whaleAmount = d.Amount
			break
		}
	}

	// Current (buggy) behavior: whale gets floor(1_000_000 * 95_000 / 100_000)
	// = 950_000, i.e. 95% of the bucket. We assert >= 94% to leave a small
	// integer-division headroom while still proving "no cap" (a MaxStakeShareBps
	// clamp at e.g. 50% would land the whale at ~500_000, far below 0.94*bucket).
	threshold := int64(0.94 * float64(bucketHBD))
	if whaleAmount < threshold {
		t.Fatalf("audit #23: expected whale distribution >= 0.94 * bucket (%d) under current uncapped behavior, got %d",
			threshold, whaleAmount)
	}

	// Sanity: the small members each get only ~1.25% of the bucket, confirming
	// the pro-rata math is unmodified by any clamp today.
	for _, d := range out {
		if d.Account == "hive:whale" {
			continue
		}
		if d.Amount > bucketHBD/10 {
			t.Fatalf("audit #23: small member %s should be far below 10%% of bucket today, got %d",
				d.Account, d.Amount)
		}
	}

	// Post-fix the differential flips: ComputeNodeDistributions (or its caller
	// in compose.go:96) should clamp each account's share to
	// MaxStakeShareBps/10000 * bucketHBD. With a 50% cap the whale would land
	// at 500_000 and the residual (450_000) would either be redistributed to
	// the uncapped members or left in ResidualHBD for the next epoch. The
	// assertion above would then become `whaleAmount <= 0.50 * bucketHBD`.
}
