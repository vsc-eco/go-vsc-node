// Package delegationmode defines the operator-controlled consensus-delegation
// mode published in a node's announcement and stored on its witness record.
// It is a dependency-free leaf package so it can be imported by the
// announcement, witness DB, state-processing, ledger, and GraphQL layers
// without creating import cycles.
//
// The mode lets a node operator opt in to (or out of) third-party consensus
// delegation to their node, and choose how pendulum rewards earned on that
// delegated bond are distributed. It is consensus-relevant only at consensus
// 0.3.0+ (the same gate as per-delegator stake/unstake): the stake-time
// enforcement and the settlement reward split both read it, so every node must
// agree on the value. Because it is sourced from the operator's own
// authenticated Hive `update_account` announcement (json_metadata), the value
// is exactly what the operator signed.
package delegationmode

import "strings"

const (
	// Deactivated is the default: no NEW third-party delegation is accepted by
	// this node (a consensus_stake where from != to is rejected). The operator
	// can always self-stake (from == to), and any stake already delegated to
	// the node (incl. migrated pre-0.3.0 stake) remains reclaimable by the
	// delegator — unstake is never gated by the mode. This makes delegation a
	// genuine opt-in: an operator who has not announced a mode accepts no
	// delegations.
	Deactivated = "deactivated"

	// Share accepts third-party delegation AND splits the pendulum rewards
	// earned on the node's bond pro-rata across every stake edge (the
	// operator's own self-stake edge included), in relation to each staker's
	// stake. No separate operator commission is taken on-chain — the operator
	// earns purely via their own self-stake edge. Operators who want a cut of
	// delegator rewards should use Custom with an off-chain agreement.
	Share = "share"

	// Custom accepts third-party delegation but pays all pendulum rewards to
	// the node operator account (today's behaviour); any revenue share with
	// delegators is settled off-chain under a private agreement. On-chain this
	// is identical to the legacy reward path.
	Custom = "custom"
)

// Default is the mode assumed when a node has not announced one (or announced
// an empty / unrecognized value). Deactivated makes delegation strictly
// opt-in.
const Default = Deactivated

// Normalize maps an arbitrary announced string to a known mode, defaulting to
// Default for empty / unrecognized input. Case- and whitespace-insensitive so
// a stray "Share " in a config file still resolves. Deterministic: same input
// → same output on every node, which is required because the result gates
// consensus-relevant stake acceptance and reward distribution.
func Normalize(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case Share:
		return Share
	case Custom:
		return Custom
	case Deactivated:
		return Deactivated
	default:
		return Default
	}
}

// IsValid reports whether s is exactly one of the canonical mode strings (no
// normalization). Used to validate operator config at load time so a typo
// surfaces instead of silently falling back to Deactivated.
func IsValid(s string) bool {
	switch s {
	case Deactivated, Share, Custom:
		return true
	default:
		return false
	}
}

// AllowsDelegation reports whether a third-party account may delegate consensus
// stake to a node running mode s. Both Share and Custom accept delegation;
// Deactivated does not. The operator's own self-stake is always allowed by the
// caller regardless of mode and does not go through this check.
func AllowsDelegation(s string) bool {
	return Normalize(s) != Deactivated
}

// SharesRewards reports whether pendulum rewards on a node's bond are split
// pro-rata to its delegators on-chain (Share mode only). Custom and Deactivated
// pay the operator account and leave any delegator share to off-chain
// settlement.
func SharesRewards(s string) bool {
	return Normalize(s) == Share
}
