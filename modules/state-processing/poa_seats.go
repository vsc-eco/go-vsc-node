package state_engine

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"vsc-node/modules/common/consensusversion"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/poaseats"
	"vsc-node/modules/db/vsc/witnesses"

	"go.mongodb.org/mongo-driver/mongo"
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

	// ★ THE TRANSITION IS DETECTED BEFORE THE ROW COUNT, NOT AFTER IT.
	//
	// An empty registry has two very different causes. One is "the batch just
	// activated and nothing has seeded yet" — the case bootstrap exists for.
	// The other is "this node LOST its registry": poa_seats is not part of any
	// merklized state and the reindex trigger keys off a hive_blocks marker, so
	// the collection can be dropped or restored independently of chain history.
	// Treating those two as the same thing means a node that loses its registry
	// silently re-seeds from whatever the CURRENT committee happens to be, and
	// then disagrees with every peer that still holds the true, continuously
	// maintained one — a divergence with no checkpoint or repair path anywhere
	// to catch it. So re-seeding is allowed ONLY at the transition: the ratified
	// election whose PREDECESSOR was still below the POA line.
	//
	// That much was already true. What changed is WHERE the check sits.
	//
	// It used to sit inside `if len(seats) == 0`, which made the whole bootstrap
	// unreachable the moment the registry held even one row. That is precisely
	// the state a crash leaves behind: AdmitSeat writes one row at a time
	// (bootstrapPoaSeats below), while the streamer only checkpoints AFTER
	// s.process() returns. Kill a node midway through seeding N members and it
	// restarts, REPLAYS the same block, finds len(seats) == k ≠ 0, skips
	// bootstrap forever, and falls into the maintenance loop below — which
	// iterates the k rows that exist and therefore can never write the missing
	// N-k. The registry is then permanently short, permanently divergent from
	// every peer, and, because seats are append-only with no delete path and the
	// transition fires exactly once, permanently unrepairable.
	//
	// Hoisting the transition test above the row count makes bootstrap
	// REPLAY-COMPLETE instead of once-only. It is safe to re-run because
	// AdmitSeat is idempotent per account: a duplicate is a deterministic,
	// typed error (isDuplicateSeatErr) that every node sees identically and that
	// the seeding loop treats as "already present", not as a failure. Re-running
	// is therefore convergent — it can only ever add the seats this same
	// ratified election already specifies, never remove or alter one.
	//
	// The maintenance loop is skipped on this path deliberately: at the
	// transition the seats being written ARE this election's members, so every
	// seat is seated and none has exited. There is nothing for it to do.
	// ★ THE UNCONDITIONAL RE-RUN REQUIRES A *KNOWN* PREDECESSOR BELOW THE LINE.
	//
	// Note what is NOT folded in here: `prevElection == nil`. A nil predecessor
	// means "this node cannot see the previous election", which is not evidence
	// of a transition — it is absence of evidence. While the row count guarded
	// the whole block that distinction was harmless, because a populated
	// registry short-circuited it. With the row count gone, folding nil in would
	// make EVERY election with an unreadable predecessor re-seed from whatever
	// committee happens to be current, quietly adding accounts that were never
	// voted in. That turns the allowlist into a rubber stamp, which is the exact
	// failure the append-only registry exists to prevent. Nil is handled below,
	// where an empty registry still makes seeding the only sensible response.
	prevKnownBelowPoa := prevElection != nil &&
		!consensusversion.PoaSeatGateActive(elections.ResultVersion(*prevElection))

	if prevKnownBelowPoa {
		if len(seats) > 0 {
			log.Warn("poa: activation transition replayed against a NON-EMPTY seat registry — re-running bootstrap so a partially-seeded registry is completed rather than frozen short; seats already present are left untouched",
				"epoch", elecResult.Epoch, "height", blockHeight, "existing_seats", len(seats))
		}
		se.bootstrapPoaSeats(elecResult, blockHeight, members)
		return
	}

	if len(seats) == 0 {
		// Registry empty and the predecessor is unreadable: seeding is the only
		// response that can ever bring POA up, and it cannot lose information
		// because there is none to lose.
		if prevElection == nil {
			se.bootstrapPoaSeats(elecResult, blockHeight, members)
			return
		}
		log.Error("poa: seat registry is EMPTY but this is not the activation transition — NOT re-seeding. This node has most likely lost its poa_seats collection and is now inconsistent with its peers; restore it or full-reindex. The seat gate stays inert meanwhile",
			"epoch", elecResult.Epoch, "height", blockHeight)
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

	// ★ RG-1 COMPLETE CLOSE (both the first-election AND the re-election gap).
	//
	// The halt exists to keep a bond slashable for as long as its operator can
	// SIGN — i.e., can be in the committee. The seat lifecycle fields
	// (LastSeatedHeight/ExitHeight) only update at RATIFICATION, so they LAG the
	// [generation, ratification) window in which an election is decided but not
	// yet on-chain. Gating solely on those fields left two gaps in that window:
	//
	//   - first-election gap: a freshly-admitted seat (LastSeatedHeight==0) about
	//     to be seated for the first time;
	//   - re-election gap: a seat that was seated, left, and whose halt window
	//     has ELAPSED (released), now about to be re-seated.
	//
	// In BOTH, the attacker must be an ELECTABLE WITNESS at unstake time (that is
	// the only way to win the pending election), so electability — not the
	// lagging seat clock — is the correct trigger. `isElectableWitness` reads the
	// exact set the election proposer draws candidates from
	// (GetWitnessesAtBlockHeight + EnabledOnly, deterministic and freshness-
	// filtered), so if the operator can be put into the next committee, the bond
	// is held. This is a pure function of on-chain state at `height`.
	window := se.sconf.ConsensusParams().EffectivePoaExitHalt()

	if se.isElectableWitness(account, height) {
		return true
	}

	// ★ RG-1c CLOSE (the disable-in-the-gap variant). isElectableWitness samples
	// the CURRENT candidate set, but an attacker can be electable at the pending
	// election's GENERATION height and disable its witness before UNSTAKING, so a
	// point-in-time "electable now" check reads false while it is about to be
	// seated from the frozen generation membership.
	//
	// The fix does not need to know the (off-chain) generation membership: the
	// generation→ratification window is a handful of blocks, while the halt
	// window is ~3 days. So if the account has had ANY witness activity — enabled
	// OR just-disabled — within the last `window` blocks, an election it was in
	// may still be in flight, and the bond is held. Only once it has been
	// witness-silent for a FULL window (no announcement activity) is it provable
	// that no in-flight election can seat it, and the seat clock may govern
	// release. `window` (>= the announcement-freshness horizon on mainnet) is far
	// larger than any generation→ratification gap, so this leaves no timing seam.
	if se.hadRecentWitnessActivity(account, height, window) {
		return true
	}

	// From here the operator has been provably non-electable for a full window —
	// it cannot be placed in any committee, in-flight or future — so it is
	// genuinely wound down and the bond releases on the seat clock.

	if seat.LastSeatedHeight == 0 {
		// Never seated AND no longer electable → release after `window` measured
		// from admission. This is the release path a never-seated seat previously
		// LACKED (the F2 freeze-forever): an admitted operator who never served
		// disables its witness and, one window later, may withdraw. It cannot
		// re-open the first-election gap, because a would-be attacker in that gap
		// IS an electable witness and so was already held above.
		release := seat.AdmittedHeight + window
		if release < seat.AdmittedHeight {
			return true // overflow guard
		}
		return height < release
	}
	if seat.ExitHeight == 0 {
		// Seated, not electable, but no exit recorded yet — the next election
		// will record the exit and start the clock. Hold until then.
		return true
	}

	release := seat.ExitHeight + window
	if release < seat.ExitHeight {
		// Overflow — only reachable via an absurd configured halt. Hold rather
		// than wrap to a release height in the past.
		log.Error("poa exit-halt: release height overflowed; HOLDING",
			"account", account, "exit_height", seat.ExitHeight)
		return true
	}
	return height < release
}

// isElectableWitness reports whether account is a candidate the election
// proposer would draw from at height: it reads the EXACT set the proposer uses
// (GetWitnessesAtBlockHeight + EnabledOnly — enabled, fresh within the
// announcement window, height-addressed and therefore deterministic). If the
// operator can be placed into the next committee, its bond must stay slashable.
//
// Fail-CLOSED: a nil witness store or a read error returns true (assume
// electable → hold), matching the halt's fail-closed posture — a delayed
// withdrawal is bounded, a released bond on a signer is not.
func (se *StateEngine) isElectableWitness(account string, height uint64) bool {
	if se.witnessDb == nil {
		return true
	}
	ws, err := se.witnessDb.GetWitnessesAtBlockHeight(height, witnesses.EnabledOnly())
	if err != nil {
		log.Error("poa exit-halt: witness read failed; HOLDING (fail-closed)",
			"account", account, "height", height, "err", err)
		return true
	}
	bare := poaseats.NormalizeAccount(account)
	for _, w := range ws {
		if poaseats.NormalizeAccount(w.Account) == bare {
			return true
		}
	}
	return false
}

// hadRecentWitnessActivity reports whether account made ANY witness announcement
// (enabled or disabled) within the last `window` blocks. A recently-disabled
// witness may still be a member of an in-flight election (decided at generation,
// not yet ratified), so its bond must stay held until the account has been
// witness-silent for a full window — long enough that any such election has
// ratified. Fail-CLOSED (nil store or read error → true → hold).
func (se *StateEngine) hadRecentWitnessActivity(account string, height, window uint64) bool {
	if se.witnessDb == nil {
		return true
	}
	h := height
	w, err := se.witnessDb.GetWitnessAtHeight(poaseats.NormalizeAccount(account), &h)
	if err != nil {
		// A genuine "no announcement" (ErrNoDocuments) means the account has
		// never been a witness → not electable, no in-flight election. Any other
		// error is a transient read → fail closed.
		if errors.Is(err, mongo.ErrNoDocuments) {
			return false
		}
		log.Error("poa exit-halt: witness-activity read failed; HOLDING (fail-closed)",
			"account", account, "height", height, "err", err)
		return true
	}
	if w == nil {
		return false
	}
	// w.Height < height (GetWitnessAtHeight filters height<bh), so no underflow.
	return height-w.Height < window
}

// PoaExitHaltReleaseHeight returns the height at which an account's exit-halt
// lifts, and whether a fixed release height exists. It mirrors IsPoaExitHalted:
// while the operator is still electable — currently, or via recent witness
// activity that may leave an election in flight — there is NO fixed release, so
// it returns armed=false and the caller phrases the refusal accordingly.
func (se *StateEngine) PoaExitHaltReleaseHeight(account string, height uint64) (uint64, bool) {
	if se.poaSeats == nil || se.sconf == nil {
		return 0, false
	}
	seat, found, err := se.poaSeats.GetSeat(account)
	if err != nil || !found {
		return 0, false
	}
	window := se.sconf.ConsensusParams().EffectivePoaExitHalt()
	if se.isElectableWitness(account, height) || se.hadRecentWitnessActivity(account, height, window) {
		return 0, false // held while electable / recently active — no fixed release
	}
	if seat.LastSeatedHeight == 0 {
		return seat.AdmittedHeight + window, true
	}
	if seat.ExitHeight == 0 {
		return 0, false // exit not yet recorded
	}
	return seat.ExitHeight + window, true
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

	// ★ PROOF-OF-POSSESSION IS CHECKED HERE, NOT ONLY IN THE ELECTION GATE.
	//
	// The election gate (H-6) is the usual place this is enforced, and it is
	// switched off in production. That alone would be a liveness tradeoff. What
	// makes it a permanence problem is this function: whatever is elected at the
	// transition is written into a registry that is APPEND-ONLY BY DESIGN — there
	// is no Delete, no Revoke, and no vote can remove a seat, because a set that
	// can shrink is a cheaper set to capture. So an operator who cannot
	// demonstrate control of the key it announced does not merely serve one bad
	// epoch; it becomes a permanent member of the electorate, holding a share of
	// the ceil(2/3) veto over every future admission.
	//
	// Checking here also removes the runbook from the critical path. Because
	// MeetsConsensusMin is >=, raising a floor arms every batch at or below it at
	// once, so "activate key-strict before POA" is an ordering that lives only in
	// an operator procedure and cannot be expressed in the version numbers. An
	// in-batch check means that even a floor raised straight past both batches in
	// one step still seeds a PoP-clean registry.
	//
	// A MISSING key or PoP is rejected exactly like an invalid one. Tolerating
	// absence would make the check trivially bypassable — announce no PoP and
	// walk through it — which is no check at all. The consequence is a real
	// precondition on activation, and it is stated rather than hidden: every
	// founding operator must be running a binary that announces both PoPs and
	// must have re-announced before the floor rises. The election proposer's
	// shadow evaluation exists to measure that before the fact.
	if se.witnessDb != nil {
		unproven := make([]string, 0)
		proven := make(map[string]struct{}, len(members))
		for acct := range members {
			// Reuses the package's existing fail-stop witness read rather than
			// repeating it. That helper already draws the one distinction this
			// call cannot get wrong: GetWitnessAtHeight signals "no announcement
			// below this height" with mongo.ErrNoDocuments, and blockingRetry
			// returns only on a nil error — so a hand-rolled version that passed
			// that error straight through would spin forever on the first member
			// whose record is merely absent, wedging the node at this block with
			// no election and no progress. Absence is determinate: no record
			// means no proof. A genuinely transient failure still blocks, because
			// reading a blip as "unproven" would drop a legitimate operator from
			// a registry that has no delete path.
			w := se.getWitnessAtHeightOrBlock(acct, blockHeight)
			if w == nil {
				unproven = append(unproven, acct+"(no witness record)")
				continue
			}
			if err := w.VerifyConsensusPoP(); err != nil {
				unproven = append(unproven, acct+"(consensus PoP: "+err.Error()+")")
				continue
			}
			if err := w.VerifyGatewayKeyPoP(); err != nil {
				unproven = append(unproven, acct+"(gateway PoP: "+err.Error()+")")
				continue
			}
			proven[acct] = struct{}{}
		}

		if len(unproven) > 0 {
			slices.Sort(unproven)
			log.Error("poa: bootstrap EXCLUDING committee members that cannot prove possession of their announced keys — seats are permanent, so an unproven key must not be enshrined",
				"epoch", elecResult.Epoch, "height", blockHeight,
				"elected", len(members), "proven", len(proven), "unproven", len(unproven),
				"excluded", strings.Join(unproven, ","))
		}

		// Re-check the floor against what SURVIVED, not what was elected. The
		// check above is upstream of the same reasoning: seeding a set too small
		// to widen itself is unrecoverable, and so is refusing to seed. Refusing
		// is the direction that keeps the chain running (the gate stays inert and
		// candidacy continues exactly as before), so it is the one to take when
		// the two are in tension.
		if minMembers := se.sconf.ConsensusParams().MinMembers; minMembers > 0 && len(proven) < minMembers {
			log.Error("poa: bootstrap REFUSED — too few of the incumbent committee can prove possession of their announced keys to found the operator set. Registry stays EMPTY and the seat gate stays INERT; the chain is unaffected but POA does not activate. Every founding operator must announce a valid consensus-key AND gateway-key proof-of-possession before the consensus floor is raised",
				"epoch", elecResult.Epoch, "height", blockHeight,
				"proven", len(proven), "min_members", minMembers,
				"unproven", strings.Join(unproven, ","))
			return
		}
		members = proven
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
	var reseeded []string
	for _, acct := range accounts {
		// Fail-stop like every other registry write on this path. This is the
		// once-ever seeding burst, so a transient DB blip here desyncs one
		// node's registry from its peers permanently — and because bootstrap
		// only fires at the transition, nothing ever re-runs it.
		var err error
		var alreadyPresent bool
		blockingRetry(fmt.Sprintf("poaSeats.AdmitSeat(bootstrap,%s,%d)", acct, blockHeight), func() error {
			alreadyPresent = false
			err = se.poaSeats.AdmitSeat(poaseats.Seat{
				Account:          acct,
				AdmittedHeight:   blockHeight,
				Bootstrap:        true,
				LastSeatedHeight: blockHeight,
			})
			// A duplicate-key error is DETERMINISTIC (every node sees it) and
			// must not be retried forever; only infra errors are transient.
			if err != nil && isDuplicateSeatErr(err) {
				alreadyPresent = true
				return nil
			}
			return err
		})
		// ★ A DUPLICATE IS A SUCCESS, NOT A FAILURE.
		//
		// The closure above already stops retrying on a duplicate, but `err` is
		// the OUTER variable and is still non-nil, so without this the account
		// was counted as `failed`. That was cosmetic only while bootstrap could
		// run at most once per network. It is not cosmetic now: bootstrap is
		// replay-complete, so the ordinary case on any replay of the transition
		// block is that EVERY account is a duplicate. Left as-is, a healthy
		// replay would report the entire committee as failed and fire the
		// partial-bootstrap alarm below every single time — and an alarm that
		// cries wolf on the happy path is an alarm the operator learns to
		// ignore, which is worse than not having one.
		if alreadyPresent {
			reseeded = append(reseeded, acct)
			seeded = append(seeded, acct)
			continue
		}
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
		"already_present", len(reseeded),
		"newly_written", len(seeded)-len(reseeded),
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
