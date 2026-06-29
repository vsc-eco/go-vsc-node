// Package governance holds the deterministic, dependency-free core of the
// witness-vote governance engine: vote tallying, the supermajority threshold,
// beneficiary exclusion, and proposal expiry math.
//
// The whole point of this package is that it is PURE — given the same inputs it
// returns the same outputs on every node, with no DB, clock, or network access.
// That is what lets the state engine tally L1-broadcast votes during block
// replay and reach an identical approve/expire decision everywhere (the same
// determinism property the slash itself relies on). The persistence layer
// (modules/db/vsc/governance) and the L1 op handlers (state-processing) wrap
// this; they never re-implement the math.
package governance

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

// ProposalType enumerates the governance actions tallied through the shared
// witness-vote engine. The string values are persisted and appear in derived
// proposal ids, so they are part of the on-chain contract — do not rename.
type ProposalType string

const (
	// ProposalSlashRestore is the light path: cancel a pending safety-slash and
	// re-credit the HIVE_CONSENSUS bond while the slash is still in its pending
	// window (see applySafetySlashReverse, action "both").
	ProposalSlashRestore ProposalType = "slash_restore"
	// ProposalReservePayout is the heavy path: debit the keyless insurance
	// reserve and credit a named recipient. The general make-whole mechanism for
	// matured slashes and theft victims.
	ProposalReservePayout ProposalType = "reserve_payout"
)

// ProposalStatus is the lifecycle state of a proposal row.
type ProposalStatus string

const (
	// StatusOpen: accepting votes, not yet approved or expired.
	StatusOpen ProposalStatus = "open"
	// StatusApplied: crossed the threshold and the ledger effect was applied.
	// Terminal and idempotent — further votes are no-ops.
	StatusApplied ProposalStatus = "applied"
	// StatusExpired: reached its effective expiry without approval. Terminal.
	StatusExpired ProposalStatus = "expired"
)

// Member is one electorate entry: a witness account and its voting weight
// (election stake weight). Built by the caller from an ElectionResult snapshot.
type Member struct {
	Account string
	Weight  uint64
}

// NormalizeAccount canonicalizes a Hive account name for comparison: trims
// surrounding space, lowercases, and strips a leading "hive:" prefix, so a vote
// op's RequiredAuths account, an election member, and a beneficiary all compare
// on the same key regardless of which surface produced them.
func NormalizeAccount(a string) string {
	a = strings.ToLower(strings.TrimSpace(a))
	a = strings.TrimPrefix(a, "hive:")
	return a
}

// EffectiveWeight sums the weights of the electorate EXCLUDING the beneficiary.
// This is the denominator basis for the threshold: the beneficiary of a payout
// never counts toward — or against — its own approval (no self-dealing). The
// exclusion is deterministic (derived from the on-chain election + the resolved
// beneficiary), so every node computes the same basis.
func EffectiveWeight(electorate []Member, beneficiary string) uint64 {
	ben := NormalizeAccount(beneficiary)
	var total uint64
	for _, m := range electorate {
		if NormalizeAccount(m.Account) == ben {
			continue
		}
		total += m.Weight
	}
	return total
}

// RequiredWeight is the minimal voted weight to approve: a BFT-safe 2/3+
// supermajority of the beneficiary-excluded electorate weight, computed as
// ceil(2W/3) = (2W+2)/3. This matches the election helper
// (MinimalRequiredElectionVotes on 0.2.0) so governance and block consensus
// share one threshold shape. Returns 0 only when the effective electorate is
// empty; IsApproved treats that as never-approved rather than auto-approve.
func RequiredWeight(electorate []Member, beneficiary string) uint64 {
	w := EffectiveWeight(electorate, beneficiary)
	if w == 0 {
		return 0
	}
	return (2*w + 2) / 3
}

// Tally sums the weights of electorate members who have cast an approve vote,
// EXCLUDING the beneficiary. voters is the set of NORMALIZED accounts that voted
// (the caller dedups and normalizes); a voter not in the electorate snapshot
// contributes nothing.
func Tally(electorate []Member, beneficiary string, voters map[string]bool) uint64 {
	ben := NormalizeAccount(beneficiary)
	var total uint64
	for _, m := range electorate {
		acct := NormalizeAccount(m.Account)
		if acct == ben {
			continue
		}
		if voters[acct] {
			total += m.Weight
		}
	}
	return total
}

// IsApproved reports whether the voted weight meets the approval threshold over
// the beneficiary-excluded electorate. It requires a strictly positive required
// weight, so a degenerate all-beneficiary (or empty) electorate can never
// auto-approve a payout to the beneficiary.
func IsApproved(electorate []Member, beneficiary string, voters map[string]bool) bool {
	req := RequiredWeight(electorate, beneficiary)
	if req == 0 {
		return false
	}
	return Tally(electorate, beneficiary, voters) >= req
}

// EffectiveExpiry returns the block height at which a proposal closes: the
// EARLIER of (creationBlock + expiryBlocks) and, when maturity != 0, the slash's
// maturity height. This is the locked "simple-cap" rule — a slash_restore window
// is min(expiry, remaining pending time) and NEVER extends the slash's fixed
// maturity. maturity == 0 means "no maturity cap" (reserve_payout, which is not
// bounded by a pending window). Overflow of creationBlock+expiryBlocks is not a
// concern at real Hive heights.
func EffectiveExpiry(creationBlock, expiryBlocks, maturity uint64) uint64 {
	exp := creationBlock + expiryBlocks
	if maturity != 0 && maturity < exp {
		return maturity
	}
	return exp
}

// IsExpired reports whether currentBlock has reached or passed the proposal's
// effective expiry. Expiry is inclusive (a proposal at exactly the expiry height
// is closed) so the boundary is unambiguous across nodes.
func IsExpired(currentBlock, creationBlock, expiryBlocks, maturity uint64) bool {
	return currentBlock >= EffectiveExpiry(creationBlock, expiryBlocks, maturity)
}

// ReservePayoutProposalID derives the deterministic id for a reserve-payout
// proposal from its content (normalized recipient, amount, reason) PLUS the
// create op's tx id. Folding in createTxID means two distinct create ops are two
// distinct proposals — voters reference this id rather than reproducing the
// content, and a one base unit difference can't silently merge votes across
// unrelated payouts. Fields are null-delimited (a byte that can't appear in a
// Hive account or an integer) so the reason text can't be crafted to collide
// with a different (recipient, amount).
func ReservePayoutProposalID(recipient string, amount int64, reason, createTxID string) string {
	h := sha256.New()
	h.Write([]byte(NormalizeAccount(recipient)))
	h.Write([]byte{0})
	h.Write([]byte(strconv.FormatInt(amount, 10)))
	h.Write([]byte{0})
	h.Write([]byte(reason))
	h.Write([]byte{0})
	h.Write([]byte(strings.TrimSpace(createTxID)))
	return string(ProposalReservePayout) + ":" + hex.EncodeToString(h.Sum(nil))
}
