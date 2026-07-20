// Package poaseats holds the POA seat registry: the on-chain allowlist of
// vetted operator accounts that may be ELECTED to the committee.
//
// It exists because entry is otherwise permissionless. A witness record is
// created by any Hive account that posts an account_update announcing
// services:["vsc.network"] with a consensus key (state_engine.go account_update
// branch); from there, enough HIVE_CONSENSUS stake is the only remaining gate on
// candidacy. Under POA, stake stops being a ticket: only a seat ratified by a
// ceil(2/3) vote of existing seats (vsc.admit_vote) can be elected.
//
// THREE PROPERTIES THIS PACKAGE MUST NEVER LOSE:
//
//  1. APPEND-ONLY. Nothing removes a seat — there is no delete method here and
//     no caller may add one. Voting is entry-only by design: signers must not be
//     able to vote each other out, because "frame an honest operator and vote
//     them out" is a set-shrink, and a smaller set is a cheaper capture (the
//     admit threshold is self-protecting ONLY while the set cannot shrink). A
//     seat leaves the ELECTED set via the ordinary election filters; the seat
//     itself persists, which is also what makes re-entry-after-a-liveness-drop
//     work with no re-vote.
//
//  2. HEIGHT-ADDRESSED. Reads are "the registry as of height H", never "the
//     registry now". Election generation is a pure function of chain state that
//     every node re-derives and must agree on to the CID; a read that returned
//     rows admitted after H would make a reindex disagree with a live node about
//     the past.
//
//  3. ACCOUNT-NORMALISED. Accounts are stored and compared BARE (no "hive:"
//     prefix). Election member records are inconsistent on this point across
//     history, so every comparison goes through NormalizeAccount. A prefix
//     mismatch here does not throw an error — it silently matches nothing, i.e.
//     it empties the committee.
package poaseats

import (
	"strings"

	a "vsc-node/modules/aggregate"
)

// Seat is one ratified POA seat. One row per operator, written once at
// admission and thereafter only updated with seating/exit bookkeeping.
type Seat struct {
	// Account is the operator's Hive account, BARE (no "hive:" prefix).
	// Always write it through NormalizeAccount.
	Account string `bson:"account"`

	// UboId identifies the ultimate beneficial owner behind this seat. It is
	// what makes "one operator, one seat" structural rather than procedural: a
	// second admission carrying a UboId that already holds a live seat is
	// refused on-chain. The chain enforces UNIQUENESS of this string; it cannot
	// and does not verify that the string is TRUE — that is the off-chain
	// KYC/UBO vetting which must happen before a vote is ever cast.
	UboId string `bson:"ubo_id"`

	// AdmittedHeight is the L1 block at which the admit-vote crossed the
	// threshold. It is the height-addressing key: a seat is part of the registry
	// at height H iff AdmittedHeight <= H.
	AdmittedHeight uint64 `bson:"admitted_height"`

	// AdmittedTxId is the L1 transaction of the vote that crossed the threshold
	// (empty for bootstrap seats).
	AdmittedTxId string `bson:"admitted_tx_id,omitempty"`

	// Bootstrap marks a seat seeded from the incumbent committee at the moment
	// the seat gate first activated, rather than admitted by a vote. Without
	// this seeding the gate would activate against an empty registry and delete
	// every candidate — the exact failure mode that halted mainnet elections at
	// epoch 1699 via the H-6 key-admission gate. Kept as a field (not inferred
	// from an empty tx id) so the distinction is auditable after the fact.
	Bootstrap bool `bson:"bootstrap,omitempty"`

	// LastSeatedHeight is the height of the most recent ratified election whose
	// member set INCLUDED this account. 0 means the seat has never been elected.
	LastSeatedHeight uint64 `bson:"last_seated_height,omitempty"`

	// ExitHeight is the height of the first ratified election that EXCLUDED this
	// account after it had been seated. 0 means "currently seated, or never
	// seated" — the two are distinguished by LastSeatedHeight.
	//
	// This field exists because the codebase otherwise has no answer to "when
	// did this account leave the set": election records carry no reason, no
	// status and no departure height, and membership is purely positional. The
	// collateral exit-halt is counted from here.
	ExitHeight uint64 `bson:"exit_height,omitempty"`
}

// Seated reports whether the seat is currently in the elected committee: it has
// been elected at least once and has not since been observed absent.
func (s Seat) Seated() bool {
	return s.LastSeatedHeight > 0 && s.ExitHeight == 0
}

// NormalizeAccount strips the optional "hive:" prefix so registry keys compare
// equal regardless of which convention the caller's source used. Election member
// records are written bare by the current proposer but historical rows carry the
// prefix, and every existing consumer in the codebase defends the same way.
func NormalizeAccount(account string) string {
	return strings.TrimPrefix(strings.TrimSpace(account), "hive:")
}

// PoaSeats is the seat registry. Note the absence of any Delete/Revoke method:
// that is property 1 above, enforced by the interface itself rather than by
// convention.
type PoaSeats interface {
	a.Plugin

	// GetSeatsAtHeight returns every seat admitted at or before height, sorted
	// by account, so callers derive an identical set on every node.
	GetSeatsAtHeight(height uint64) ([]Seat, error)

	// GetSeat returns the seat for an account (any prefix form), and whether it
	// exists. A non-nil error is a READ FAILURE and must be treated as
	// fail-stop by consensus callers — never as "no seat".
	GetSeat(account string) (Seat, bool, error)

	// GetSeatByUbo returns the live seat held by a beneficial owner, if any.
	// Backs the one-seat-per-UBO rule.
	GetSeatByUbo(uboId string) (Seat, bool, error)

	// AdmitSeat inserts a new seat. It fails if the account already holds a seat
	// or if the UboId already holds one.
	AdmitSeat(seat Seat) error

	// SetSeating records that a seat WAS in the ratified election at height:
	// LastSeatedHeight = height, ExitHeight cleared (re-entry re-arms the halt).
	SetSeating(account string, height uint64) error

	// SetExit records that a seat was ABSENT from the ratified election at
	// height, having previously been seated. Idempotent: an exit that is already
	// recorded is not moved forward, so the halt clock cannot be restarted by
	// subsequent elections.
	SetExit(account string, height uint64) error
}
