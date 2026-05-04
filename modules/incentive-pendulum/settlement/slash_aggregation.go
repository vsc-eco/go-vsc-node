package settlement

import (
	"sort"

	pendulum_oracle "vsc-node/modules/db/vsc/pendulum_oracle"
)

// AccumulateSlashBps sums each witness's slash bps across the supplied
// snapshots and returns the deterministic per-witness total. Snapshots
// should already be ordered (callers pass output of
// PendulumOracleSnapshots.GetSnapshotsInRange) but the order does not affect
// the sum — only the iteration over the returned map's seed-sorted keys is
// load-bearing.
//
// Per-witness bps are CAPPED at 10000 (100%) here so a malformed snapshot
// can't make later math overflow; the per-epoch hard cap (e.g. 1000 bps =
// 10%) is enforced at slash-application time, not here.
func AccumulateSlashBps(snapshots []pendulum_oracle.SnapshotRecord) map[string]int {
	if len(snapshots) == 0 {
		return nil
	}
	totals := make(map[string]int)
	for _, snap := range snapshots {
		for _, entry := range snap.WitnessSlashBps {
			if entry.Witness == "" || entry.Bps <= 0 {
				continue
			}
			totals[entry.Witness] += entry.Bps
			if totals[entry.Witness] > 10000 {
				totals[entry.Witness] = 10000
			}
		}
	}
	if len(totals) == 0 {
		return nil
	}
	return totals
}

// SortedSlashAccounts returns the keys of an accumulated slash map in
// lexicographic order — the canonical iteration order for building a
// SettlementPayload's slash list.
func SortedSlashAccounts(totals map[string]int) []string {
	if len(totals) == 0 {
		return nil
	}
	out := make([]string, 0, len(totals))
	for k := range totals {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
