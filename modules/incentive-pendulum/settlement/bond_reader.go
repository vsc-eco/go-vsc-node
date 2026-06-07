package settlement

import (
	"fmt"
	"strings"

	"vsc-node/lib/vsclog"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
)

var log = vsclog.Module("pendulum-settlement")

// BalanceRecordReader is the narrow read surface this package needs from
// ledgerDb.Balances. Pulled out so tests can stub without standing up Mongo.
type BalanceRecordReader interface {
	GetBalanceRecord(account string, blockHeight uint64) (*ledgerDb.BalanceRecord, error)
}

// bondSampleCount is the number of point-in-time samples ReadCommitteeBonds
// draws across (epochStartBh, slotHeight] when computing each member's
// effective bond. The window minimum across the samples is taken as the
// effective bond, so a flash-stake at slotHeight−k for small k gets
// credited the pre-flash balance (worst-case 0). Higher counts tighten the
// approximation at the cost of more ledger reads per settlement.
//
// With ~1200-block epoch windows and a committee of ~21, 8 samples ⇒ ~170
// reads per settlement — well under the per-block budget but enough to
// catch any flash-stake larger than ~150 blocks of dwell time. Adversaries
// can still pre-position over the full window, but at that point the
// "flash" attack collapses into "stake honestly for the whole epoch".
const bondSampleCount = 8

// ReadCommitteeBonds returns the per-account effective HIVE_CONSENSUS bond
// across the window (epochStartBh, slotHeight], reading directly from
// BalanceRecord.HIVE_CONSENSUS instead of via
// LedgerSystem.GetBalance("hive_consensus").
//
// The effective bond is min(HIVE_CONSENSUS_t) sampled at bondSampleCount
// equally-spaced points across the window. The min-form is a conservative
// TWAB stand-in: it defangs the "flash-stake at slotHeight" front-run that
// the point-in-time snapshot was vulnerable to (audit #122) and is
// reproducible across nodes because every sample is a deterministic
// at-or-before-height read from the ledger.
//
// Why the snapshot field (BalanceRecord.HIVE_CONSENSUS) directly:
// CORRECTION (audit R4): the prior comment here claimed
// GetBalance("hive_consensus") applies an op-type filter that excludes
// consensus_stake and "silently returns 0 for freshly-staked HIVE". That is
// FALSE — GetBalance's hive_consensus arm sums all four consensus mutators
// (consensus_stake / consensus_unstake / safety_slash_consensus[_reverse],
// ledger_state.go), so it DOES count fresh stake; the {"unstake","deposit"}
// filter the old comment cited is the HBD arm, not hive_consensus. The
// snapshot-field read is retained here as the settlement record's existing
// behaviour. Whether to move settlement onto the replay read (more complete /
// less sparse — audit X2; the election bond gate already uses the replay via
// LedgerState.GetConsensusBalanceAt) is a SEPARATE consensus-affecting change
// to the settlement CID and is intentionally NOT made in this comment fix.
//
// Accounts in the returned map are normalized to "hive:account" form so the
// caller can correlate with slash payloads (which also use that form).
// Accounts with zero (or all-zero-window) bond are omitted so callers can
// iterate the map and only see the subset that actually contributes to T_post.
//
// FAIL-STOP on read error (audit GAP-1): a per-node transient ledger read error
// is non-deterministic; silently dropping the errored sample (the old
// behaviour) let one node compute a higher effective bond than its peers →
// divergent settlement map → settlement CID fork. On ANY sample read error this
// returns (nil, err) so the caller abstains (the producer aborts the election
// attempt; the verifier returns no-record) — matching the election bond gate,
// which fail-stops on the same error class. A genuinely-empty result (no member
// bonded, degenerate/genesis window) still returns (nil, nil).
func ReadCommitteeBonds(reader BalanceRecordReader, members []string, epochStartBh, slotHeight uint64) (map[string]int64, error) {
	if reader == nil || len(members) == 0 {
		return nil, nil
	}
	if slotHeight == 0 {
		// Genesis / uninitialized — no meaningful read possible.
		return nil, nil
	}
	if slotHeight <= epochStartBh {
		// Degenerate window — degrade gracefully to a single-point read at
		// slotHeight so callers without a meaningful window (genesis, tests)
		// still get a usable bond map. Same behaviour as the pre-fix code.
		epochStartBh = slotHeight - 1
	}

	samples := sampleBlocksAcrossWindow(epochStartBh, slotHeight, bondSampleCount)
	out := make(map[string]int64, len(members))
	for _, m := range members {
		acct := normalizeHiveAccount(m)
		if acct == "" {
			continue
		}
		minBond, ok, err := readMinBondAcrossSamples(reader, acct, samples)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if minBond <= 0 {
			continue
		}
		out[acct] = minBond
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// sampleBlocksAcrossWindow returns count block heights spanning (start, end],
// inclusive of end so the existing slot-end read is always covered. Sample
// spacing is deterministic given (start, end, count) so every honest node
// produces identical sample sets.
func sampleBlocksAcrossWindow(start, end uint64, count int) []uint64 {
	if count < 2 {
		return []uint64{end}
	}
	if end <= start+1 {
		return []uint64{end}
	}
	width := end - start
	step := width / uint64(count-1)
	if step == 0 {
		step = 1
	}
	out := make([]uint64, 0, count)
	seen := make(map[uint64]struct{}, count)
	for i := 0; i < count; i++ {
		bh := start + step*uint64(i)
		if bh > end {
			bh = end
		}
		if bh <= start {
			bh = start + 1
		}
		if _, dup := seen[bh]; dup {
			continue
		}
		seen[bh] = struct{}{}
		out = append(out, bh)
	}
	if _, dup := seen[end]; !dup {
		out = append(out, end)
	}
	return out
}

// readMinBondAcrossSamples queries the reader at each sample block and returns
// the minimum HIVE_CONSENSUS seen. ok=false means every sample was a (missing)
// zero row → caller drops the member. A non-nil error means a sample READ
// failed: that is non-deterministic across nodes, so it is propagated (NOT
// swallowed) to fail-stop the whole settlement bond read (audit GAP-1) — a
// dropped sample could otherwise hide the window minimum on one node only and
// fork the settlement CID.
func readMinBondAcrossSamples(reader BalanceRecordReader, acct string, samples []uint64) (int64, bool, error) {
	var (
		min     int64
		haveMin bool
	)
	for _, bh := range samples {
		rec, err := reader.GetBalanceRecord(acct, bh)
		if err != nil {
			return 0, false, fmt.Errorf("bond read failed for %s at block %d: %w", acct, bh, err)
		}
		if rec == nil {
			// Missing record at this height means the account had no
			// balance row at-or-before bh — a DETERMINISTIC zero (identical on
			// every node) — treat as zero bond for the purpose of min, since a
			// missing row in a TWAB window is indistinguishable from "wasn't
			// bonded yet".
			min = 0
			haveMin = true
			continue
		}
		if !haveMin || rec.HIVE_CONSENSUS < min {
			min = rec.HIVE_CONSENSUS
			haveMin = true
		}
	}
	return min, haveMin, nil
}

func normalizeHiveAccount(a string) string {
	a = strings.TrimSpace(a)
	if a == "" {
		return ""
	}
	if strings.HasPrefix(a, "hive:") {
		return a
	}
	return "hive:" + a
}
