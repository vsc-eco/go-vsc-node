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
	// ★ omitempty IS LOAD-BEARING, not style. The unique index on this field is
	// SPARSE, and a Mongo sparse index only exempts documents where the field is
	// ABSENT — an explicitly-stored empty string is indexed like any other value.
	// Without omitempty every bootstrap seat (which has no vetted owner yet)
	// would write ubo_id:"" and collide with the previous one, so seeding the
	// incumbent committee would insert exactly ONE seat and silently drop the
	// rest. The registry would then be non-empty (so the seat gate's
	// inert-while-empty guard would not fire) and one seat wide, which starves
	// the committee below MinMembers and halts elections — the exact failure this
	// whole bootstrap mechanism exists to prevent.
	UboId string `bson:"ubo_id,omitempty"`

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
	// ★ NO omitempty — and for the OPPOSITE reason to UboId above. This field is
	// QUERIED BY VALUE (SetSeating filters last_seated_height $lte, SetExit
	// filters $gt 0). In MongoDB a value query does not match a document where
	// the field is ABSENT — only an explicit null or $exists:false does — so with
	// omitempty a freshly-admitted seat, whose value is 0 and therefore omitted,
	// would match NEITHER filter. SetSeating would silently never fire for a
	// voted-in seat, so it would never be recorded as seated, never acquire an
	// exit, and never be halted. Storing an explicit 0 is what makes those
	// queries mean what they say.
	LastSeatedHeight uint64 `bson:"last_seated_height"`

	// ExitHeight is the height of the first ratified election that EXCLUDED this
	// account after it had been seated. 0 means "currently seated, or never
	// seated" — the two are distinguished by LastSeatedHeight.
	//
	// This field exists because the codebase otherwise has no answer to "when
	// did this account leave the set": election records carry no reason, no
	// status and no departure height, and membership is purely positional. The
	// collateral exit-halt is counted from here.
	// ★ NO omitempty, same reason: SetExit filters on exit_height == 0 to make
	// the exit write idempotent-once. With omitempty a fresh seat has no
	// exit_height field, that filter matches nothing, and NO seat ever gets an
	// exit recorded — which silently disables the entire collateral exit-halt
	// while every unit test using an in-memory double still passes.
	ExitHeight uint64 `bson:"exit_height"`
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
// It lowercases for the same reason: seats are WRITTEN through
// governance.NormalizeAccount (which lowercases) on the admission path, and READ
// through this helper on the election-gate and seat-maintenance paths. Two
// helpers disagreeing on case would mean a seat stored as "alice" is looked up
// as "Alice" and silently matches nothing — excluding an admitted operator from
// the committee, and recording a currently-seated operator as having EXITED,
// which arms the collateral halt against an honest node. Hive enforces lowercase
// account names at L1, so this is defence in depth rather than a live bug; the
// point is that the safety of a membership comparison must not rest on an
// external invariant that nothing here states or tests.
// ORDER MATTERS: lowercase FIRST, then strip the prefix — the same order
// governance.NormalizeAccount uses. Stripping first is case-SENSITIVE, so
// "HIVE:alice" would keep its prefix and normalise to "hive:alice" here while
// governance normalises it to "alice": the two helpers would disagree on exactly
// the input that mixed-case handling exists to cover.
func NormalizeAccount(account string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(account)), "hive:")
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
