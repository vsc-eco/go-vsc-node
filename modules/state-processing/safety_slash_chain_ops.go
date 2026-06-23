package state_engine

import (
	"math"
	"strings"

	"vsc-node/modules/common/params"
	dbTransactions "vsc-node/modules/db/vsc/transactions"
	safetyslash "vsc-node/modules/incentive-pendulum/safety_slash"
	ledgerSystem "vsc-node/modules/ledger-system"

	ledgerDb "vsc-node/modules/db/vsc/ledger"
)

// maxLedgerScanHeight is the upper block-height bound for the safety-slash reverse
// lookups below. It must NOT be ^uint64(0) (math.MaxUint64): the MongoDB ledger
// store marshals a uint64 that exceeds math.MaxInt64 into a BSON int64, which
// two's-complement-wraps to -1, so a `block_height $lte ^uint64(0)` filter
// matches ZERO rows on a real node. (Pure-Go mock ledgers used in unit tests do
// not marshal through BSON, so the bug is invisible there — it only manifests on
// a live MongoDB-backed node, e.g. the devnet integration test, where it made
// every wrongful-slash reversal silently drop with "no matching
// safety_slash_consensus row".) math.MaxInt64 is a valid BSON int64 and dwarfs
// any real Hive block height, so it is a safe, correct upper bound.
const maxLedgerScanHeight uint64 = math.MaxInt64

// safetySlashConsensusRowID rebuilds the deterministic Id used by
// SafetySlashConsensusBond for the principal-debit row. Mirrors the Go
// pattern in ledger_system.go::SafetySlashConsensusBond:
//
//	baseID = "<txID>#safety_slash#<kind>"
//	id     = baseID + "#consensus_debit#" + acct
//
// where acct is the validator's normalized "hive:<name>" account
// (Owner field of the row; Asset is "hive_consensus", not encoded in
// the Owner). Used by applySafetySlashReverse to cheaply look up the
// original slash row by exact-id match.
func safetySlashConsensusRowID(slashTxID, evidenceKind, slashedAcctNormalized string) string {
	return slashTxID + "#safety_slash#" + evidenceKind + "#consensus_debit#" + slashedAcctNormalized
}

// findSafetySlashConsensusRow reads the principal debit ledger row for a
// (slashTxID, evidenceKind, slashedAccount) tuple. Returns the row + its
// absolute amount in HIVE_CONSENSUS sats; the second return is false when
// no matching row is found.
func (se *StateEngine) findSafetySlashConsensusRow(slashTxID, evidenceKind, slashedAccount string) (ledgerDb.LedgerRecord, bool) {
	if se == nil || se.LedgerState == nil || se.LedgerState.LedgerDb == nil {
		return ledgerDb.LedgerRecord{}, false
	}
	owner := normalizeHiveAccount(slashedAccount)
	wantID := safetySlashConsensusRowID(slashTxID, evidenceKind, owner)
	recs, err := se.LedgerState.LedgerDb.GetLedgerRange(
		owner,
		0,
		maxLedgerScanHeight,
		"hive_consensus",
		ledgerDb.LedgerOptions{OpType: []string{ledgerSystem.LedgerTypeSafetySlashConsensus}},
	)
	if err != nil || recs == nil {
		return ledgerDb.LedgerRecord{}, false
	}
	for _, r := range *recs {
		if r.Id == wantID {
			return r, true
		}
	}
	return ledgerDb.LedgerRecord{}, false
}

// alreadyFinalizedToReserveAmt sums this slash's residual that has been
// COMMITTED to the keyless reserve (destination-change-only) — either landed
// directly (BurnDelayBlocks==0, Id "<baseID>#reserve#<acct>") or promoted from
// pending on maturity (Id "<baseID>#hive_burn_pending#<acct>#promoted_to_reserve").
// Both Ids keep the "<slashTxID>#safety_slash#<kind>#" prefix. (Pre-reserve this
// read the burn account; the residual no longer burns.)
//
// Used by applySafetySlashReverse: once a residual is committed to the reserve it
// is a retained backstop (NOT destroyed), so a bond reverse must NOT also
// re-credit it — that would MINT value (bond restored AND the residual still
// sits in the reserve). The reverse headroom subtracts this. (Burning the
// residual did not have this problem because burnt value is gone; a recoverable
// reserve does, which is exactly why the reverse cap had to change.)
func (se *StateEngine) alreadyFinalizedToReserveAmt(slashTxID, evidenceKind string) int64 {
	if se == nil || se.LedgerState == nil || se.LedgerState.LedgerDb == nil {
		return 0
	}
	// Block-on-error, not return-0 (audit R2-F2): this is subtracted from the
	// reverse headroom; a 0 from a transient DB error would inflate headroom and
	// let a reverse over-credit the bond while the residual still sits in the
	// reserve (a mint), and could fork. Block until committed state is readable.
	var recs *[]ledgerDb.LedgerRecord
	blockingRetry("alreadyFinalizedToReserveAmt", func() error {
		var err error
		recs, err = se.LedgerState.LedgerDb.GetLedgerRange(
			params.ProtocolSlashReserveAccount,
			0,
			maxLedgerScanHeight,
			"hive",
			ledgerDb.LedgerOptions{OpType: []string{ledgerSystem.LedgerTypeSafetySlashReserve}},
		)
		return err
	})
	if recs == nil {
		return 0
	}
	prefix := slashTxID + "#safety_slash#" + evidenceKind + "#"
	total := int64(0)
	for _, r := range *recs {
		if strings.HasPrefix(r.Id, prefix) {
			total += r.Amount
		}
	}
	return total
}

// uncancelledPendingResidualAmt sums this slash's residual still sitting in the
// PENDING burn account and NOT yet cancelled or matured — i.e. value that is
// in-flight to the reserve. Net of LedgerTypeSafetySlashHiveBurnPending credits
// minus LedgerTypeSafetySlashHiveBurnPendingRelease debits (a cancel or a
// maturity-promotion each writes a release that nets the pending row to 0).
// Both row Ids keep the "<slashTxID>#safety_slash#<kind>#" prefix.
//
// Used by applySafetySlashReverse (audit R2-F1): the reverse cap must treat
// in-flight pending residual as COMMITTED, exactly like landed reserve. Without
// this, a reverse-only action DURING the challenge window (residual still
// pending, not yet promoted) would re-credit the full bond, and then the pending
// residual would still mature into the reserve — bond restored AND reserve holds
// it (an overstatement / latent mint). Counting the live pending here forces a
// during-window undo to go through cancel+reverse ("both"), where the cancel
// nets the pending to 0 so the reverse can legitimately restore the bond.
func (se *StateEngine) uncancelledPendingResidualAmt(slashTxID, evidenceKind string) int64 {
	if se == nil || se.LedgerState == nil || se.LedgerState.LedgerDb == nil {
		return 0
	}
	var recs *[]ledgerDb.LedgerRecord
	blockingRetry("uncancelledPendingResidualAmt", func() error {
		var err error
		recs, err = se.LedgerState.LedgerDb.GetLedgerRange(
			params.ProtocolSlashPendingBurnAccount,
			0,
			maxLedgerScanHeight,
			"hive",
			ledgerDb.LedgerOptions{OpType: []string{
				ledgerSystem.LedgerTypeSafetySlashHiveBurnPending,
				ledgerSystem.LedgerTypeSafetySlashHiveBurnPendingRelease,
			}},
		)
		return err
	})
	if recs == nil {
		return 0
	}
	prefix := slashTxID + "#safety_slash#" + evidenceKind + "#"
	total := int64(0)
	for _, r := range *recs {
		if strings.HasPrefix(r.Id, prefix) {
			total += r.Amount // credits positive, releases negative; net = live residual
		}
	}
	if total < 0 {
		total = 0
	}
	return total
}

// alreadyReversedConsensusAmt sums prior reverse credits for a slash
// tuple, so a second reverse op cannot double-credit the validator.
func (se *StateEngine) alreadyReversedConsensusAmt(slashTxID, evidenceKind, slashedAccount string) int64 {
	if se == nil || se.LedgerState == nil || se.LedgerState.LedgerDb == nil {
		return 0
	}
	owner := normalizeHiveAccount(slashedAccount)
	// Block-on-error, not return-0 (audit R2-F2): subtracted from the reverse
	// headroom; a 0 from a transient DB error would let a second reverse
	// double-credit the bond, and could fork. Block until readable.
	var recs *[]ledgerDb.LedgerRecord
	blockingRetry("alreadyReversedConsensusAmt", func() error {
		var err error
		recs, err = se.LedgerState.LedgerDb.GetLedgerRange(
			owner,
			0,
			maxLedgerScanHeight,
			"hive_consensus",
			ledgerDb.LedgerOptions{OpType: []string{ledgerSystem.LedgerTypeSafetySlashConsensusReverse}},
		)
		return err
	})
	if recs == nil {
		return 0
	}
	prefix := slashTxID + "#safety_slash#" + evidenceKind + "#consensus_reverse#"
	total := int64(0)
	for _, r := range *recs {
		if strings.HasPrefix(r.Id, prefix) {
			total += r.Amount
		}
	}
	return total
}

// ApplySafetySlashReverseForTest exposes applySafetySlashReverse for the
// external state_engine_test package. The block dispatcher path is
// covered by the same code; this wrapper exists solely so external
// tests can drive the apply path without needing a full BLS-signed
// block fixture.
func (se *StateEngine) ApplySafetySlashReverseForTest(rec safetyslash.SafetySlashReverseRecord, carryingTxID string, blockHeight uint64) {
	se.applySafetySlashReverse(rec, carryingTxID, blockHeight)
}

// NewChainOpStateEngineForTest builds a minimal StateEngine wired with
// caller-supplied LedgerSystem + LedgerState + txDb so external tests can
// exercise the chain-op apply functions end-to-end without needing the full
// plugin constellation that newStateEngine assembles. Production code never
// calls this constructor.
func NewChainOpStateEngineForTest(ls ledgerSystem.LedgerSystem, state *ledgerSystem.LedgerState, txDb dbTransactions.Transactions) *StateEngine {
	return &StateEngine{
		LedgerSystem:                  ls,
		LedgerState:                   state,
		txDb:                          txDb,
		safetyEvidenceSeen:            make(map[string]uint64),
		seenProposalBySlotProposer:    make(map[string]string),
		slashIncidentBpsBySlotAccount: make(map[string]int),
	}
}

// applySafetySlashReverse is the BlockTypeSafetySlashReverse handler.
// Per the no-explicit-window auth model, the 2/3 BLS aggregate over the
// carrying VSC block is the timelock; this handler performs ledger
// consistency checks so governance cannot mint past the original debit:
//
//  1. The referenced safety_slash_consensus row exists.
//  2. For cancel: CancelPendingSafetySlashBurn rejects automatically if
//     the pending row has been finalized or already cancelled.
//  3. For reverse: amount is capped at slashAmount - committedToReserve
//     (landed reserve + in-flight pending) - alreadyReversed, so the net
//     re-credit never exceeds the portion not yet committed to the
//     reserve (destination-change correctness — see the headroom block
//     below).
//  4. For both: the cancel runs first; the reverse amount is the smaller
//     of the requested amount and the post-cancel headroom.
func (se *StateEngine) applySafetySlashReverse(
	rec safetyslash.SafetySlashReverseRecord,
	carryingTxID string,
	blockHeight uint64,
) {
	if se == nil || se.LedgerSystem == nil {
		return
	}
	rec = rec.Normalize()

	if rec.SlashTxID == "" || rec.EvidenceKind == "" || rec.SlashedAccount == "" {
		log.Warn("safety slash reverse: missing required field; dropping",
			"slash_tx_id", rec.SlashTxID, "kind", rec.EvidenceKind,
			"account", rec.SlashedAccount)
		return
	}
	switch rec.Action {
	case safetyslash.ReverseActionCancel,
		safetyslash.ReverseActionReverse,
		safetyslash.ReverseActionBoth:
	default:
		log.Warn("safety slash reverse: unsupported action; dropping",
			"action", rec.Action, "slash_tx_id", rec.SlashTxID)
		return
	}

	slashRow, ok := se.findSafetySlashConsensusRow(rec.SlashTxID, rec.EvidenceKind, rec.SlashedAccount)
	if !ok {
		log.Warn("safety slash reverse: no matching safety_slash_consensus row; dropping",
			"slash_tx_id", rec.SlashTxID, "kind", rec.EvidenceKind,
			"slashed_account", rec.SlashedAccount)
		return
	}
	slashAmt := slashRow.Amount
	if slashAmt <= 0 {
		log.Warn("safety slash reverse: slash row has non-positive abs amount; dropping",
			"slash_tx_id", rec.SlashTxID, "abs_amt", slashAmt)
		return
	}

	if rec.Action == safetyslash.ReverseActionCancel || rec.Action == safetyslash.ReverseActionBoth {
		cancelRes := se.LedgerSystem.CancelPendingSafetySlashBurn(ledgerSystem.CancelPendingSafetySlashBurnParams{
			TxID:           rec.SlashTxID,
			EvidenceKind:   rec.EvidenceKind,
			SlashedAccount: rec.SlashedAccount,
			BlockHeight:    blockHeight,
			Reason:         rec.Reason,
		})
		if cancelRes.Ok {
			log.Info("safety slash reverse: pending burn cancelled",
				"slash_tx_id", rec.SlashTxID, "kind", rec.EvidenceKind)
		} else if rec.Action == safetyslash.ReverseActionCancel {
			// Cancel-only and the cancel was a no-op (already finalized
			// or already cancelled) — log and stop. Reverse-or-both
			// fall through to the credit path; we still want to issue
			// the credit even if cancel was a no-op.
			log.Warn("safety slash reverse: cancel rejected by ledger system; reverse path skipped",
				"slash_tx_id", rec.SlashTxID, "msg", cancelRes.Msg)
			return
		}
	}

	if rec.Action == safetyslash.ReverseActionReverse || rec.Action == safetyslash.ReverseActionBoth {
		if rec.Amount <= 0 {
			log.Warn("safety slash reverse: non-positive amount on reverse action; dropping credit",
				"slash_tx_id", rec.SlashTxID, "amount", rec.Amount)
			return
		}
		alreadyReversed := se.alreadyReversedConsensusAmt(rec.SlashTxID, rec.EvidenceKind, rec.SlashedAccount)
		// Reverse headroom (destination-change correctness): a bond re-credit may
		// only restore the portion of the slash NOT committed anywhere. The only
		// committed destination is the reserve:
		//   - committedToReserve: residual already in the keyless reserve
		//     (landed) PLUS residual still in the pending account in-flight to the
		//     reserve and not yet cancelled (uncancelledPendingResidualAmt, audit
		//     R2-F1). Because the reserve RETAINS value (unlike a burn, which
		//     destroyed it), re-crediting the bond for EITHER would MINT — bond
		//     restored AND the value still ends up in the reserve. Counting the
		//     in-flight pending is what forces a during-window undo to use
		//     cancel+reverse ("both"): the cancel nets the pending to 0 so the
		//     reverse can then legitimately restore the bond. A reverse-only during
		//     the window correctly gets headroom 0.
		// Subtract it, clamp at 0. Conservative: can only ever UNDER-credit (a
		// wrongful slash is fully reversible during the window via cancel+reverse;
		// once committed to the reserve it is a backstop), never over-credit — the
		// safe direction for money. All readers block-on-DB-error (deterministic).
		committedToReserve := se.alreadyFinalizedToReserveAmt(rec.SlashTxID, rec.EvidenceKind) +
			se.uncancelledPendingResidualAmt(rec.SlashTxID, rec.EvidenceKind)
		headroom := slashAmt - alreadyReversed - committedToReserve
		if headroom <= 0 {
			log.Warn("safety slash reverse: nothing reversible (committed to reserve or already reversed); dropping credit",
				"slash_tx_id", rec.SlashTxID, "slash_amt", slashAmt,
				"already_reversed", alreadyReversed, "committed_to_reserve", committedToReserve)
			return
		}
		amount := rec.Amount
		if amount > headroom {
			amount = headroom
		}
		credRes := se.LedgerSystem.ReverseSafetySlashConsensusDebit(ledgerSystem.ReverseSafetySlashConsensusDebitParams{
			TxID:         rec.SlashTxID,
			EvidenceKind: rec.EvidenceKind,
			Account:      rec.SlashedAccount,
			Amount:       amount,
			BlockHeight:  blockHeight,
			Reason:       rec.Reason,
			// Distinct ops on the same slash get distinct row Ids via
			// the carrying chain-op tx id; replays of the same op upsert.
			OpInstanceID: carryingTxID,
		})
		if credRes.Ok {
			log.Info("safety slash reverse: bond credit recorded",
				"slash_tx_id", rec.SlashTxID, "amount", amount)
		} else {
			log.Warn("safety slash reverse: credit rejected by ledger system",
				"slash_tx_id", rec.SlashTxID, "msg", credRes.Msg)
		}
	}
}
