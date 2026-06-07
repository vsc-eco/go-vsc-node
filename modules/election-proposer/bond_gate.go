package election_proposer

// Bond inclusion gate wiring (audit CP-2b-ii / v2 §13.1 Stage 2) — the
// impure, DB-touching companions to bond_maturity.go's pure maturity math:
//
//   - the established-member exception: a witness that recently served as a
//     ratified member keeps its already-ratified stake exempt from the window
//     through an absence grace. This also carries the live committee across the
//     gate's own activation (the sitting committee are recent ratified members),
//     so no separate activation-snapshot/grandfather is needed;
//   - the committee floor + incumbent backfill (C2/C3 floor guard): the gate
//     must never shrink the committee below what gateway rotation and TSS
//     signing need, and may only back-fill from prior-election incumbents —
//     NEVER arbitrary unmatured stake (audit R3/H8: a guard that admits
//     unmatured stake when the committee is thin is itself an attack lever).
//
// Every input here is on-chain (ratified elections, replayed balances at
// barriered heights) or compile-time params, so every honest node computes the
// identical result at the same election height (Constraint 3). Every DB error
// fail-stops the election attempt (F4/H-10) — never a silent default.

import (
	"math"
	"slices"
	"strings"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/witnesses"
)

// bondGatewayKeyFloor mirrors the hardcoded gateway multisig floor
// (modules/gateway/multisig.go keyRotation: `len(gatewayKeys) < 8` skips
// rotation). The committee floor below must never let the gate produce an
// election the gateway cannot rotate to. Keep in sync with multisig.go —
// duplicated as a const because importing modules/gateway here would be a
// dependency inversion (gateway consumes ratified elections).
const bondGatewayKeyFloor = 8

// bondCommitteeFloor is the minimum committee size the bond gate may produce:
// max of
//   - MinMembers (a valid election needs it anyway — HoldElection aborts below
//     it, which would stall the epoch: no rotation, no reshare),
//   - the gateway multisig key floor (8 — below it keyRotation is silently
//     skipped and custody handover wedges, audit H-5/C-2),
//   - ceil(2·prevMembers/3)+1 — one more than the TSS-signable quorum of the
//     PREVIOUS committee (GetThreshold(N)+1 = ceil(2N/3) participants sign;
//     keeping at least that many prior-committee-scale seats keeps the
//     share-holding incumbents engaged so outstanding keys stay signable and
//     reshares complete, audit C-3). This also acts as a one-epoch shrink
//     bound: a committee of N can lose at most N − (ceil(2N/3)+1) seats per
//     election to the gate.
func bondCommitteeFloor(minMembers int, prevMembers int) int {
	floor := minMembers
	if bondGatewayKeyFloor > floor {
		floor = bondGatewayKeyFloor
	}
	if prevMembers > 0 {
		tssFloor := (2*prevMembers+2)/3 + 1
		if tssFloor > floor {
			floor = tssFloor
		}
	}
	return floor
}

// bondBackfillCandidate is a prior-election incumbent eligible to be re-seated
// by the floor guard, with the (capped) weight it would re-enter at.
type bondBackfillCandidate struct {
	witness witnesses.Witness
	// weight = min(current point-in-time balance, weight ratified in the
	// previous election) — the same safe-by-construction cap the gate uses
	// everywhere: never more than held now, never more than already ratified.
	weight uint64
	// effective is the gate's effective stake for this account (used only for
	// deterministic ordering: highest-matured first).
	effective int64
	// prevWeight is the account's ratified weight in the previous election
	// (ordering tiebreak).
	prevWeight uint64
}

// selectBondBackfill deterministically orders backfill candidates —
// highest effective (matured) stake first, then highest previous ratified
// weight, then account ascending — and returns the first `need` of them.
// Pure function of its inputs so every node selects the identical set.
func selectBondBackfill(cands []bondBackfillCandidate, need int) []bondBackfillCandidate {
	if need <= 0 || len(cands) == 0 {
		return nil
	}
	slices.SortFunc(cands, func(a, b bondBackfillCandidate) int {
		if a.effective != b.effective {
			if a.effective > b.effective {
				return -1
			}
			return 1
		}
		if a.prevWeight != b.prevWeight {
			if a.prevWeight > b.prevWeight {
				return -1
			}
			return 1
		}
		return strings.Compare(a.witness.Account, b.witness.Account)
	})
	if need > len(cands) {
		need = len(cands)
	}
	return cands[:need]
}

// bondEstablishedInfo records, for an account that has served as a ratified
// committee member, its MOST-RECENT membership height (for the absence-grace
// check) and the weight it was ratified for there (the exemption cap).
type bondEstablishedInfo struct {
	lastMembershipHeight uint64
	lastWeight           uint64
}

// buildBondEstablishedMap scans the previous elections (most-recent first,
// epoch DESC as returned by GetPreviousElections) and records, per account, its
// MOST-RECENT ratified membership height + weight. Used by the established-member
// exception: a witness present here served >= 1 epoch, and if that membership
// is within the absence grace it keeps its already-ratified stake exempt from
// the inclusion window. Deterministic (on-chain ratified elections, barriered).
// Pure — takes the already-read election slice so the DB read + its error
// handling live at the (fail-stop) call site.
func buildBondEstablishedMap(prevs []elections.ElectionResult) map[string]bondEstablishedInfo {
	out := make(map[string]bondEstablishedInfo)
	for _, e := range prevs {
		for i, m := range e.Members {
			acct := strings.TrimPrefix(m.Account, "hive:")
			if _, seen := out[acct]; seen {
				// epoch DESC ⇒ the first time we see an account is its most
				// recent membership; keep that, ignore older ones.
				continue
			}
			var w uint64
			if i < len(e.Weights) {
				w = e.Weights[i]
			}
			out[acct] = bondEstablishedInfo{
				lastMembershipHeight: e.BlockHeight,
				lastWeight:           w,
			}
		}
	}
	return out
}

// bondEstablishedLookback is how many past elections to scan so that an
// established member whose most-recent membership sits ANYWHERE within the
// absence grace is always found. A missed member would be wrongly gated (a
// reset — which the exception must NEVER cause), so scan the grace span in
// elections plus a generous buffer, bounded to keep the read finite.
func bondEstablishedLookback(graceBlocks, electionInterval uint64) int {
	if electionInterval == 0 {
		electionInterval = 1
	}
	n := int(graceBlocks/electionInterval) + 16
	if n < 1 {
		n = 1
	}
	// DoS backstop only — must stay HIGH enough that it never truncates the
	// grace for any sane config, or a member whose most-recent membership sits
	// inside the configured grace but beyond the cap would be missed → gated →
	// RESET (the one thing the exception must never do). 2048 elections is >1
	// year on mainnet (interval 7200 ⇒ ~426 days), so a 2-week grace (56
	// elections) is nowhere near it; it only bounds an absurd misconfiguration.
	// If BondInclusionEstablishedGraceBlocks is ever raised above
	// 2048*ElectionInterval, raise this cap in lockstep.
	const maxLookback = 2048
	if n > maxLookback {
		n = maxLookback
	}
	return n
}

// bondWithinEstablishedGrace reports whether an account is currently an
// established member: it has a ratified-membership record (served >= 1 epoch)
// AND its most-recent membership is within graceBlocks of electionHeight (gone
// no longer than the absence grace). Pure + deterministic.
func bondWithinEstablishedGrace(info bondEstablishedInfo, ok bool, electionHeight, graceBlocks uint64) bool {
	if !ok || graceBlocks == 0 || info.lastWeight == 0 {
		return false
	}
	// lastMembershipHeight is a PAST election (<= electionHeight); guard
	// underflow defensively.
	if info.lastMembershipHeight > electionHeight {
		return false
	}
	return electionHeight-info.lastMembershipHeight <= graceBlocks
}

// bondEstablishedExemption returns the established-member exempt stake for an
// account at electionHeight: min(currentBalance, last-ratified weight) when the
// account is within the established grace, else 0. The cap means it can ONLY
// restore already-earned, already-ratified power — never fast-track new stake
// (a top-up above last-ratified is excluded; the matured term still gates
// that). info is the per-account record from buildBondEstablishedMap; current
// is the point-in-time balance (>=0).
func bondEstablishedExemption(info bondEstablishedInfo, ok bool, electionHeight, graceBlocks uint64, current int64) int64 {
	if !bondWithinEstablishedGrace(info, ok, electionHeight, graceBlocks) {
		return 0
	}
	cap := uint64ToInt64Clamped(info.lastWeight)
	if current < cap {
		return current
	}
	return cap
}

// uint64ToInt64Clamped converts a ratified uint64 weight to int64 for
// comparison against int64 balances, clamping at MaxInt64 (ratified weights
// originate from uint64(positive int64) casts so the clamp is defensive only).
func uint64ToInt64Clamped(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}

// bondChurnEntrant is a NEW member (not in the previous election) competing for
// one of the limited new-member seats under the F6 churn cap.
type bondChurnEntrant struct {
	account string
	// effective is the WINDOW-MATURED stake ONLY (audit A6-1) — NOT the
	// established-inflated bondEffective. The ranking key: the highest
	// genuinely-matured new entrants are admitted first, so fresh unmatured
	// capital (even a returning member wearing its already-ratified weight)
	// cannot jump the churn queue ahead of honestly-matured stakers.
	effective int64
}

// selectChurnedOut returns the set of NEW members (by account) that the churn
// cap EXCLUDES this election: given `newEntrants` (every account in the current
// committee that was NOT in the previous election) and `maxNew` (the cap), it
// keeps the top `maxNew` by effective stake (desc), then account (asc) for a
// deterministic total order, and returns the REMAINDER as a set to delete.
// maxNew<=0 means "no cap" → nothing churned out. Pure function of its inputs
// (on-chain-derived effective values + compile-time cap) ⇒ identical on every
// node (Constraint 3).
func selectChurnedOut(newEntrants []bondChurnEntrant, maxNew int) map[string]struct{} {
	if maxNew <= 0 || len(newEntrants) <= maxNew {
		return nil
	}
	slices.SortFunc(newEntrants, func(a, b bondChurnEntrant) int {
		if a.effective != b.effective {
			if a.effective > b.effective {
				return -1
			}
			return 1
		}
		return strings.Compare(a.account, b.account)
	})
	out := make(map[string]struct{}, len(newEntrants)-maxNew)
	for _, e := range newEntrants[maxNew:] {
		out[e.account] = struct{}{}
	}
	return out
}
