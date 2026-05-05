package settlement

import (
	pendulum_oracle "vsc-node/modules/db/vsc/pendulum_oracle"
	"vsc-node/modules/incentive-pendulum/rewards"
)

// AccumulateRewardReductionBps returns the per-witness reward reduction bps
// for an epoch, computed by summing each witness's per-tick consolidated bps
// across the supplied snapshots, subtracting the per-epoch forgiveness
// buffer, and clamping at the per-epoch cap.
//
// Snapshots must already be ordered (callers pass output of
// PendulumOracleSnapshots.GetSnapshotsInRange) but the order does not affect
// the sum.
//
// The returned map omits witnesses whose effective bps is 0 (either because
// they accumulated nothing or the buffer absorbed everything). This is the
// canonical input for ApplyRewardReductionsToBonds.
func AccumulateRewardReductionBps(snapshots []pendulum_oracle.SnapshotRecord) map[string]int {
	return rewards.AggregateEpoch(snapshots)
}

// SortedReductionAccounts is a thin re-export of the rewards package helper
// so existing callers in this package don't have to import rewards/.
func SortedReductionAccounts(totals map[string]int) []string {
	return rewards.SortedReductionAccounts(totals)
}
