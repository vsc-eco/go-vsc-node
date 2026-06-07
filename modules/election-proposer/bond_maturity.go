package election_proposer

import (
	"fmt"

	ledgerDb "vsc-node/modules/db/vsc/ledger"
)

// Bond inclusion window (audit CP-2 / v2 §13.2) — the core of the 3-day stake
// maturity gate. A witness's stake only counts toward governance once it has
// been held continuously for the inclusion window, giving the network a
// reaction window before newly-acquired stake can influence the election
// committee (which feeds block production, the gateway multisig, and the TSS
// committee). The gate is applied at the single election chokepoint
// (GenerateFullElection); this file holds only the deterministic, side-effect-
// free maturity computation so it can be unit-tested in isolation.

// balanceReplayReader is the read surface the maturity gate needs: the
// REPLAY-based hive_consensus balance (LedgerState.GetConsensusBalanceAt), NOT
// the snapshot field (BalanceRecord.HIVE_CONSENSUS). The audit (R4 / m1b-2 /
// m23-5) proved the snapshot field is sparse + stale, while the replay read
// is both complete (counts fresh consensus_stake) and identical across
// nodes/reindexes. The read is ERROR-AWARE (audit F4/H-10, z3 m63-6): a seat
// gate must FAIL-STOP on a DB read error — silently treating an error as
// 0/stale evicts an honest member on one node's transient hiccup and forks the
// election CID across nodes. *stateEngine.StateEngine's LedgerState satisfies
// this interface.
type balanceReplayReader interface {
	GetConsensusBalanceAt(account string, blockHeight uint64) (int64, error)
	// ConsensusReverseCreditsInRange returns the safety_slash_consensus_reverse
	// rows in [start,end] for the reverse-slash grandfather (X1). Error-aware
	// (fail-stop, same as the balance read).
	ConsensusReverseCreditsInRange(account string, start, end uint64) ([]ledgerDb.LedgerRecord, error)
}

// maturityWindowStart returns the (saturating) start height of the inclusion
// window for an election at electionHeight with window length w (w>0; the w==0
// "gate disabled" case is handled by the caller, NOT here). Saturating
// subtraction is MANDATORY (v2 §10 / Fear-1): a raw electionHeight-w underflows
// to ~1.8e19 when electionHeight < w (early chain / low devnet heights), which
// would push every sample above the head, read 0, and permanently lock every
// witness out. When electionHeight <= w (not enough history yet) the window
// clamps to (0, electionHeight] — "must have been staked since genesis", the
// strictest honest reading at low heights, which relaxes naturally as the chain
// grows past w.
func maturityWindowStart(electionHeight, w uint64) uint64 {
	if w == 0 || electionHeight <= w {
		return 0
	}
	return electionHeight - w
}

// maturedConsensusStake returns the matured HIVE_CONSENSUS bond for account at
// electionHeight: the MINIMUM replayed hive_consensus balance sampled across
// the maturity window (maturityWindowStart(electionHeight,w), electionHeight].
// The min-over-window is the gate: freshly-staked or recently-increased HIVE
// reads its lower (often 0) value at the early samples, so it does not count
// until held continuously for the whole window; an account that starts
// unstaking loses weight immediately (the min drops at once). Reads go through
// the error-aware replay read (see balanceReplayReader). Pure function of
// on-chain reads + compile-time params ⇒ identical on every honest node at the
// same electionHeight (Constraint 3). Callers MUST barrier the state engine to
// electionHeight first (CP-0a) so every sample is settled.
//
// ANY read error returns a non-nil error and the caller MUST fail-stop (abort
// the whole election attempt; the next slot retries). Audit F4/H-10, z3 m63-6:
// fail-stop is the unique non-divergent response — error-as-zero evicts an
// honest member on one node only (CID fork), error-as-skip silently weakens
// the gate.
//
// w==0 is the explicit kill-switch: it returns the true point-in-time current
// balance (no maturity requirement). It must short-circuit BEFORE the sampler —
// routing w==0 through the min-over-(0,electionHeight] window would impose the
// strictest "held since genesis" gate (the opposite of disabled) and could lock
// out every recent staker (scrutiny B1 / Fear-1).
//
// Known approximation (M1): with a sparse sampleCount an attacker can hold
// >= MinStake only at the (deterministic, publicly-derivable) sample heights and
// 0 between and still pass. For a SEAT gate this is acceptable — they must keep
// capital staked across ~100% of the window and gain no earlier seat (there is
// no per-block reward to skim); it only lets them hide a mid-window dip.
// Densify sampleCount if a tighter continuous-dwell guarantee is ever needed.
func maturedConsensusStake(
	reader balanceReplayReader,
	account string,
	electionHeight, w uint64,
	sampleCount int,
) (int64, error) {
	if reader == nil {
		// The gate being active with no reader is a wiring bug, not a "treat as
		// zero" condition — fail-stop (the dead-safety-param class).
		return 0, fmt.Errorf("bond maturity: nil balance reader")
	}
	if electionHeight == 0 {
		return 0, nil
	}
	if w == 0 {
		// Gate disabled — true point-in-time current balance (scrutiny B1).
		v, err := reader.GetConsensusBalanceAt(account, electionHeight)
		if err != nil {
			return 0, err
		}
		if v < 0 {
			return 0, nil
		}
		return v, nil
	}
	// A single sample is a point-in-time read that defeats the gate (fresh stake
	// would count immediately). Never let a mis-set or zero-value
	// BondInclusionSampleCount silently disable the window (scrutiny B2 / the
	// dead-safety-param class) — floor at 2.
	if sampleCount < 2 {
		sampleCount = 2
	}
	windowStart := maturityWindowStart(electionHeight, w)
	samples := sampleMaturityWindow(windowStart, electionHeight, sampleCount)

	// Reverse-slash grandfather (audit X1/H-17). Fetch every
	// safety_slash_consensus_reverse credit in the window. For a sample at
	// height s, the reversal at height h_r is NOT yet in the replayed balance
	// when s < h_r, but the erroneous slash debit it undoes WAS — so that
	// sample under-reads the exonerated bond by the reversed amount. Add the
	// sum of all such later reverses back to each sample. This surgically
	// cancels the slash dip: samples in [slash, reverse) are restored to the
	// pre-slash bond; samples that pre-date the slash get the credit added too
	// but are higher and so never win the MIN (the min picks the restored dip
	// value, not the inflated pre-slash value); fresh stake / top-ups /
	// unstakes are untouched (independent of the reverse amount).
	//
	// TWO invariants make this safe-by-construction — BOTH are load-bearing:
	//
	//  (1) UPSTREAM CAP (coupling invariant — do NOT break it): every
	//      safety_slash_consensus_reverse row's Amount is capped at the REAL
	//      prior slash it reverses. applySafetySlashReverse
	//      (state-processing/safety_slash_chain_ops.go) DROPS a reverse with no
	//      matching safety_slash_consensus row, and clamps the credit to
	//      `headroom = slashAmt - alreadyReversed`, so cumulative reverses can
	//      never exceed slashAmt, and a slash only ever lands on a bond the node
	//      ALREADY held (and matured, since you can only be slashed as an active
	//      committee member). Therefore the add-back can only ever restore
	//      previously-matured stake — it can NEVER fast-track NEW stake. If that
	//      cap is ever loosened (an over-sized or unmatched reverse becomes
	//      writable), this add-back becomes a direct maturity-gate bypass
	//      (a node could count currently-held fresh stake instantly). The gate
	//      cannot independently re-derive "how much was legitimately slashed"
	//      (the slash may pre-date the window), so there is NO clean local
	//      substitute for this upstream cap — it must hold.
	//  (2) CURRENT-BOND CEILING: sampleMaturityWindow always includes the
	//      electionHeight sample, which receives NO add-back (no reverse has
	//      height > electionHeight), so its adjusted value equals the true
	//      current bond. The MIN is therefore always <= current bond — the
	//      add-back can never inflate matured stake above what the node holds
	//      right now.
	//
	// Dormant until SafetySlashEnabled (safety_slash/policy.go); with slashing
	// off there are no reverse rows so this is a no-op (one extra empty read).
	reverses, rErr := reader.ConsensusReverseCreditsInRange(account, windowStart, electionHeight)
	if rErr != nil {
		return 0, rErr
	}

	var (
		min  int64
		have bool
	)
	for _, bh := range samples {
		v, err := reader.GetConsensusBalanceAt(account, bh)
		if err != nil {
			return 0, err
		}
		for _, rc := range reverses {
			if rc.BlockHeight > bh && rc.Amount > 0 {
				v += rc.Amount
			}
		}
		if !have || v < min {
			min = v
			have = true
		}
	}
	if !have {
		return 0, nil
	}
	if min < 0 {
		// HIVE_CONSENSUS is floored at >=0 by its mutators; defense-in-depth so
		// a negative never propagates into the uint64 weight cast downstream.
		return 0, nil
	}
	return min, nil
}

// sampleMaturityWindow returns up to count deterministic block heights spanning
// (start, end], always including end (the election height — the most recent and
// most security-relevant point) and at least one near-start sample (so a stake
// added just after windowStart still has to wait). Spacing is pure integer
// arithmetic over (start, end, count) so every node draws the identical set.
// Mirrors the settlement sampler's contract but is kept separate: this is a
// seat gate (GetBalance replay, exclude-on-low), not a settlement TWAB.
func sampleMaturityWindow(start, end uint64, count int) []uint64 {
	if end == 0 {
		return nil
	}
	if count < 2 || end <= start+1 {
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
