package state_engine

import (
	"slices"
	"strings"

	"vsc-node/modules/common/consensusversion"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/poaseats"
)

// POA seat maintenance — the consensus point.
//
// Two jobs, both driven off a RATIFIED election rather than off election
// generation:
//
//   1. Bootstrap seeding. The seat gate is an allowlist over election
//      candidacy. Activated against an empty registry it would delete every
//      candidate and halt elections — which is not hypothetical: the
//      structurally identical H-6 key-admission gate "starved the mainnet
//      committee below the floor at epoch 1699, halting elections" and remains
//      disabled today (consensusversion.WitnessKeyStrictActive). So the first
//      ratified election after the batch activates SEEDS the registry from its
//      own member set. No operator action, no admin op, no flag day.
//
//   2. Seating / exit bookkeeping. The codebase has no record of when an
//      account left the committee: election documents carry no reason, no
//      status and no departure height, and membership is purely positional
//      (present in Members, or not). The collateral exit-halt needs that fact,
//      so it is created here, by diffing each ratified election against the
//      registry.
//
// WHY HERE AND NOT IN THE ELECTION PROPOSER: the proposer runs per-node and
// speculatively (it may generate elections that are never ratified, and it runs
// on nodes that are not the proposer). Writing consensus state from it would
// make the registry depend on which node you asked. TxElectionResult.ExecuteTx
// is the point every node executes exactly once per ratified election, from
// identical inputs — the same place StoreElection itself is called.

// applyPoaSeatMaintenance updates the seat registry from a freshly ratified
// election. Called immediately after StoreElection succeeds.
//
// Errors are logged, never fatal: a registry write failure must not abort
// election processing (that would halt the chain over bookkeeping). The
// consumers are all fail-closed instead — the seat gate goes inert on an
// unreadable registry, and the exit-halt holds rather than releases — so a
// missed write costs safety-side conservatism, never a spurious release.
func (se *StateEngine) applyPoaSeatMaintenance(elecResult elections.ElectionResult, blockHeight uint64) {
	if se.poaSeats == nil {
		return
	}
	if !consensusversion.PoaSeatGateActive(se.ActiveConsensusVersion(blockHeight)) {
		return
	}

	// Member accounts, normalised once. Election member records are written
	// bare by the current proposer but historical rows carry a "hive:" prefix;
	// comparing the two forms directly matches nothing, which in this code path
	// would mean "every seat exited at once".
	members := make(map[string]struct{}, len(elecResult.Members))
	for _, m := range elecResult.Members {
		acct := poaseats.NormalizeAccount(m.Account)
		if acct != "" {
			members[acct] = struct{}{}
		}
	}

	seats, err := se.poaSeats.GetSeatsAtHeight(blockHeight)
	if err != nil {
		// Fail-stop for the caller's purposes: do NOT proceed to seed or to
		// record exits off a partial read. Seeding off a failed read would
		// duplicate the whole committee; recording exits off one would arm the
		// halt against every operator simultaneously.
		log.Error("poa: seat registry read failed; skipping seat maintenance for this election",
			"epoch", elecResult.Epoch, "height", blockHeight, "err", err)
		return
	}

	if len(seats) == 0 {
		se.bootstrapPoaSeats(elecResult, blockHeight, members)
		return
	}

	for _, seat := range seats {
		if _, inSet := members[seat.Account]; inSet {
			if err := se.poaSeats.SetSeating(seat.Account, blockHeight); err != nil {
				log.Error("poa: failed to record seating", "account", seat.Account, "height", blockHeight, "err", err)
			}
			continue
		}
		// Absent from this election. SetExit is a no-op unless the seat had
		// previously been seated AND has no exit recorded yet, so a seat that
		// has never been elected never arms a halt, and a seat that exited long
		// ago does not have its clock restarted by every subsequent election.
		if err := se.poaSeats.SetExit(seat.Account, blockHeight); err != nil {
			log.Error("poa: failed to record exit", "account", seat.Account, "height", blockHeight, "err", err)
		}
	}
}

// IsPoaExitHalted reports whether an account's consensus bond is under the POA
// collateral exit-halt at height.
//
// THE ATTACK IT CLOSES: an operator holds threshold BTC shares. They can sign
// off-protocol and Bitcoin confirms in ~10 minutes regardless of anything Magi
// does — so theft is not prevented, it is DETERRED, by the collateral they
// forfeit. That deterrent evaporates if they can steal and pull their collateral
// out before the theft is detected. The halt parks the bond for
// PoaExitHaltBlocks counted from the moment they LEAVE the controlling set,
// which must exceed (theft-detection latency + slash-execution time).
//
// It is held while STILL SEATED too, not only after exit. A thief who steals and
// simply stays in the set, enjoying the BTC, must not be able to walk the
// collateral out the front door in the meantime.
//
// TERMINATION (this is not an indefinite seizure): unstaking drops the account's
// weight, so it leaves the set at the next election, which records an exit
// height and starts the clock. An operator who chooses to remain seated remains
// held — by their own choice, and they can end it at any time by disabling their
// witness. The refusal message says so.
//
// FAIL-CLOSED. An unreadable registry HOLDS rather than releases: the failure
// mode of holding is a delayed withdrawal, the failure mode of releasing is a
// thief's collateral leaving during the detection window. Same discipline as
// bondLockMatches (bond_lock.go), for the same reason.
func (se *StateEngine) IsPoaExitHalted(account string, height uint64) bool {
	if se.poaSeats == nil {
		return false
	}
	if !consensusversion.PoaExitHaltActive(se.ActiveConsensusVersion(height)) {
		return false
	}

	seat, found, err := se.poaSeats.GetSeat(account)
	if err != nil {
		log.Error("poa exit-halt: seat read failed; HOLDING the bond (fail-closed)",
			"account", account, "height", height, "err", err)
		return true
	}
	if !found {
		// No seat: nothing POA has any claim over.
		return false
	}
	if seat.LastSeatedHeight == 0 {
		// Admitted but never elected — never held keys, so never held collateral
		// hostage to a theft it could not have committed.
		return false
	}
	if seat.ExitHeight == 0 {
		// Still in the set: holds keys, so holds the bond.
		return true
	}

	release := seat.ExitHeight + se.sconf.ConsensusParams().EffectivePoaExitHalt()
	if release < seat.ExitHeight {
		// Overflow — only reachable via an absurd configured halt. Hold rather
		// than wrap to a release height in the past.
		log.Error("poa exit-halt: release height overflowed; HOLDING",
			"account", account, "exit_height", seat.ExitHeight)
		return true
	}
	return height < release
}

// PoaExitHaltReleaseHeight returns the height at which an account's exit-halt
// lifts, and whether one is currently armed. Used to put a concrete, checkable
// number in the refusal message rather than "try again later".
func (se *StateEngine) PoaExitHaltReleaseHeight(account string, height uint64) (uint64, bool) {
	if se.poaSeats == nil {
		return 0, false
	}
	seat, found, err := se.poaSeats.GetSeat(account)
	if err != nil || !found || seat.LastSeatedHeight == 0 || seat.ExitHeight == 0 {
		return 0, false
	}
	return seat.ExitHeight + se.sconf.ConsensusParams().EffectivePoaExitHalt(), true
}

// bootstrapPoaSeats seeds the registry from the first ratified election observed
// after the POA batch activates.
//
// Deterministic by construction: the input is the ratified election object every
// node already agrees on, and the output is one seat per member at that
// election's height. A node replaying history reaches the identical registry.
//
// Bootstrap seats carry no UboId. That is deliberate and it is a stated
// limitation, not an oversight: the incumbent committee has not been through
// KYC/UBO vetting, and recording a fabricated owner id would make the registry
// claim a fact nobody established. The empty id is sparse-indexed so bootstrap
// seats do not collide with each other, and the one-seat-per-UBO rule therefore
// binds only the seats that were actually voted in. Vetting the incumbents is an
// off-chain action that must happen before the set is treated as vetted.
func (se *StateEngine) bootstrapPoaSeats(elecResult elections.ElectionResult, blockHeight uint64, members map[string]struct{}) {
	if len(members) == 0 {
		// Nothing to seed from. Leaving the registry empty is the SAFE outcome:
		// the seat gate is inert while it is empty, so candidacy stays as it was
		// rather than the gate deleting everyone.
		log.Error("poa: bootstrap skipped — ratified election has no usable members; seat gate stays inert",
			"epoch", elecResult.Epoch, "height", blockHeight)
		return
	}

	seeded := make([]string, 0, len(members))
	for acct := range members {
		err := se.poaSeats.AdmitSeat(poaseats.Seat{
			Account:          acct,
			AdmittedHeight:   blockHeight,
			Bootstrap:        true,
			LastSeatedHeight: blockHeight,
		})
		if err != nil {
			log.Error("poa: bootstrap seat write failed", "account", acct, "height", blockHeight, "err", err)
			continue
		}
		seeded = append(seeded, acct)
	}

	// Sorted purely so the log line is stable and diffable across nodes; the
	// registry's own reads are sorted independently.
	slices.Sort(seeded)
	log.Info("poa: seat registry bootstrapped from the incumbent committee",
		"epoch", elecResult.Epoch,
		"height", blockHeight,
		"seats", len(seeded),
		"accounts", strings.Join(seeded, ","))
}
