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
