package state_engine

import (
	"errors"
	"fmt"
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
// Reads and writes here are FAIL-STOP (blockingRetry), because ExitHeight
// decides whether an unstake is refused or paid: a write that silently fails on
// one node makes that node compute a different result for the identical tx, and
// SetExit's idempotency means no later election repairs it. A stalled slot is
// recoverable; a permanently divergent registry is not. The downstream consumers
// are additionally fail-closed (the seat gate goes inert on an unreadable or
// short registry; the exit-halt holds rather than releases).
func (se *StateEngine) applyPoaSeatMaintenance(elecResult elections.ElectionResult, prevElection *elections.ElectionResult, blockHeight uint64) {
	if se.poaSeats == nil {
		return
	}
	// ★ RESOLVE THE VERSION FROM THE ELECTION BEING PROCESSED, not from
	// ActiveConsensusVersion(blockHeight).
	//
	// GetElectionByHeight filters block_height $lt height, and the election we
	// are processing was just stored AT this height — so ActiveConsensusVersion
	// here resolves to the PREVIOUS election, the very same row prevElection
	// points at. Using it above while the transition check below tests the same
	// row for the OPPOSITE verdict is a contradiction that can never be
	// satisfied: bootstrap would never fire on any node at any epoch, the
	// registry would stay empty forever, and the batch would sit permanently
	// inert while flat weight and the churn cap still applied — the exact
	// "worse than either regime" hybrid this build guards against elsewhere.
	//
	// Taking the version from elecResult is also the more honest reading: the
	// question is "is the election I am processing under POA rules", and that
	// election carries its own version.
	if !consensusversion.PoaSeatGateActive(elections.ResultVersion(elecResult)) {
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

	// Fail-stop: never proceed off a PARTIAL read. Seeding from a failed read
	// would duplicate the whole committee; recording exits from one would arm the
	// collateral halt against every operator at once. blockingRetry returns only
	// on success, so `seats` below is always a complete read.
	var seats []poaseats.Seat
	blockingRetry(fmt.Sprintf("poaSeats.GetSeatsAtHeight(%d)", blockHeight), func() error {
		var err error
		seats, err = se.poaSeats.GetSeatsAtHeight(blockHeight)
		return err
	})

	if len(seats) == 0 {
		// ★ BOOTSTRAP ONLY AT THE TRANSITION, not merely "whenever the registry
		// looks empty".
		//
		// An empty registry has two very different causes. One is "the batch just
		// activated and nothing has seeded yet" — the case bootstrap exists for.
		// The other is "this node LOST its registry": poa_seats is not part of
		// any merklized state and the reindex trigger keys off a hive_blocks
		// marker, so the collection can be dropped or restored independently of
		// chain history. Treating those two as the same thing means a node that
		// loses its registry silently re-seeds from whatever the CURRENT
		// committee happens to be, and then disagrees with every peer that still
		// holds the true, continuously-maintained one — a divergence with no
		// checkpoint or repair path anywhere to catch it.
		//
		// The transition is identifiable: it is the ratified election whose
		// PREDECESSOR was still below the POA line. Anywhere else, an empty
		// registry is an anomaly, and the safe response is to leave it empty
		// (which keeps the seat gate inert) and say so loudly.
		prevBelowPoa := prevElection == nil ||
			!consensusversion.PoaSeatGateActive(elections.ResultVersion(*prevElection))
		if !prevBelowPoa {
			log.Error("poa: seat registry is EMPTY but this is not the activation transition — NOT re-seeding. This node has most likely lost its poa_seats collection and is now inconsistent with its peers; restore it or full-reindex. The seat gate stays inert meanwhile",
				"epoch", elecResult.Epoch, "height", blockHeight)
			return
		}
		se.bootstrapPoaSeats(elecResult, blockHeight, members)
		return
	}

	// ★ THESE WRITES ARE FAIL-STOP, NOT BEST-EFFORT.
	//
	// ExitHeight decides whether an unstake is refused or paid. If a write
	// silently fails on ONE node while succeeding on its peers, that node
	// computes a different TxResult for the identical transaction — a consensus
	// divergence on a ledger-affecting decision. And because SetExit is
	// idempotent-once (the guard that correctly stops the halt clock from
	// restarting), no later election can repair a wrong or missing first write:
	// the divergence is PERMANENT, not transient.
	//
	// So a failing write blocks the slot until the DB recovers, exactly as every
	// other consensus-critical read/write in this package does (bond_lock,
	// safety-slash, the pending-action reads). A stalled slot is recoverable; a
	// silently divergent registry is not.
	for _, seat := range seats {
		if _, inSet := members[seat.Account]; inSet {
			blockingRetry(fmt.Sprintf("poaSeats.SetSeating(%s,%d)", seat.Account, blockHeight), func() error {
				return se.poaSeats.SetSeating(seat.Account, blockHeight)
			})
			continue
		}
		// Absent from this election. SetExit is a no-op unless the seat had
		// previously been seated AND has no exit recorded yet, so a seat that
		// has never been elected never arms a halt, and a seat that exited long
		// ago does not have its clock restarted by every subsequent election.
		blockingRetry(fmt.Sprintf("poaSeats.SetExit(%s,%d)", seat.Account, blockHeight), func() error {
			return se.poaSeats.SetExit(seat.Account, blockHeight)
		})
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
	if se.sconf == nil {
		// Fail CLOSED, and never panic. Sibling gates (IsBondLockedRetiringMember,
		// SafetySlashActive) guard sconf explicitly because nothing enforces that
		// sconf and the store it reads are wired together; a security gate that
		// crashes on a consensus tx-processing path is strictly worse than one
		// that holds.
		return true
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
	if se.poaSeats == nil || se.sconf == nil {
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

	// ★ FLOOR CHECK ON WHAT WE ARE ABOUT TO ENSHRINE.
	//
	// Seats are append-only and growing the set needs a ceil(2/3) vote FROM THE
	// SEATS THEMSELVES. So whatever this one election happens to contain becomes
	// a permanent lower bound on the operator set — and if the transition lands
	// on an abnormally small committee (a degraded period, a partial outage, a
	// half-recovered network), POA is permanently founded on that degraded set,
	// with the survivors holding a 2/3 veto over ever widening it again. There is
	// no un-seed.
	//
	// Refusing to seed leaves the registry empty, which keeps the seat gate inert
	// and costs nothing but another epoch of permissionless candidacy: bootstrap
	// simply retries at the next ratified election, when the committee has
	// recovered. Seeding a bad set is unrecoverable; declining to seed is not.
	if minMembers := se.sconf.ConsensusParams().MinMembers; minMembers > 0 && len(members) < minMembers {
		// NOTE, and this is a real limitation rather than a retry: the
		// transition detector keys off the PREVIOUS election being below the
		// POA line, so once this election passes, no later election is
		// recognised as the transition and bootstrap will NOT fire again. The
		// chain is unaffected (the seat gate stays inert and candidacy
		// continues exactly as today) but POA does not activate, and bringing
		// it up then needs an operator-driven seeding path that this build does
		// NOT provide. Deliberate: silently re-seeding from whatever committee
		// exists later is how nodes diverge.
		log.Error("poa: bootstrap REFUSED — the first post-activation committee is below MinMembers, and seeding it would permanently found the operator set on a degraded committee that then holds a 2/3 veto over widening it. Registry stays empty and the seat gate stays INERT. This election was the activation transition, so bootstrap will NOT retry: POA will not come up without operator intervention",
			"epoch", elecResult.Epoch, "height", blockHeight,
			"members", len(members), "min_members", minMembers)
		return
	}

	// ★ ITERATE IN SORTED ORDER, NOT MAP ORDER. Go randomises map iteration per
	// process, so seeding straight from `members` would apply writes in a
	// different order on every node. That is invisible while all writes succeed
	// — but if any subset fails, WHICH seats survive becomes node-dependent, and
	// nodes then derive different committees from different seat sets and stop
	// agreeing on elections. Consensus writes are ordered, always.
	accounts := make([]string, 0, len(members))
	for acct := range members {
		accounts = append(accounts, acct)
	}
	slices.Sort(accounts)

	seeded := make([]string, 0, len(accounts))
	var failed []string
	for _, acct := range accounts {
		// Fail-stop like every other registry write on this path. This is the
		// once-ever seeding burst, so a transient DB blip here desyncs one
		// node's registry from its peers permanently — and because bootstrap
		// only fires at the transition, nothing ever re-runs it.
		var err error
		blockingRetry(fmt.Sprintf("poaSeats.AdmitSeat(bootstrap,%s,%d)", acct, blockHeight), func() error {
			err = se.poaSeats.AdmitSeat(poaseats.Seat{
				Account:          acct,
				AdmittedHeight:   blockHeight,
				Bootstrap:        true,
				LastSeatedHeight: blockHeight,
			})
			// A duplicate-key error is DETERMINISTIC (every node sees it) and
			// must not be retried forever; only infra errors are transient.
			if err != nil && isDuplicateSeatErr(err) {
				return nil
			}
			return err
		})
		if err != nil {
			log.Error("poa: bootstrap seat write failed", "account", acct, "height", blockHeight, "err", err)
			failed = append(failed, acct)
			continue
		}
		seeded = append(seeded, acct)
	}

	// A PARTIAL bootstrap is the dangerous outcome, worse than none at all: the
	// registry is then non-empty (so the seat gate's inert-while-empty guard
	// stops protecting) but does not contain the whole committee, so the gate
	// deletes legitimate members. Shout about it at ERROR level with the exact
	// accounts, because the operator response — do not let the version floor
	// rise until this is resolved — is time-critical. The seat gate carries its
	// own independent starvation guard for exactly this case, so a partial
	// registry degrades to "gate inert" rather than to a halted chain.
	if len(failed) > 0 {
		log.Error("poa: bootstrap seeded only PART of the incumbent committee — the seat gate will refuse to apply while the registry is short; do NOT raise the consensus floor until this is resolved",
			"height", blockHeight,
			"seeded", len(seeded),
			"failed", len(failed),
			"failed_accounts", strings.Join(failed, ","))
	}
	log.Info("poa: seat registry bootstrapped from the incumbent committee",
		"epoch", elecResult.Epoch,
		"height", blockHeight,
		"seats", len(seeded),
		"accounts", strings.Join(seeded, ","))
}

// isDuplicateSeatErr reports whether a seat write failed because the seat (or
// its owner) already exists. That is a DETERMINISTIC outcome — every node
// replaying the same history sees it — so it must be surfaced, not retried:
// blockingRetry on a deterministic error wedges block processing forever.
//
// Classifies by TYPED error (errors.Is), not message text. The prior
// substring match would silently stop working if any wrapper changed the
// message, and a mis-classified duplicate is exactly what re-wedges the node.
func isDuplicateSeatErr(err error) bool {
	return errors.Is(err, poaseats.ErrSeatExists) || errors.Is(err, poaseats.ErrUboExists)
}
