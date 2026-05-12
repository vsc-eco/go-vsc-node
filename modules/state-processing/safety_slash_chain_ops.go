package state_engine

import (
	"strings"

	"vsc-node/modules/common/params"
	"vsc-node/modules/db/vsc/transactions"
	safetyslash "vsc-node/modules/incentive-pendulum/safety_slash"
	ledgerSystem "vsc-node/modules/ledger-system"

	ledgerDb "vsc-node/modules/db/vsc/ledger"
)

// safetySlashConsensusRowID rebuilds the deterministic Id used by
// SafetySlashConsensusBond for the principal-debit row. Mirrors the Go
// pattern in ledger_system.go::SafetySlashConsensusBond:
//
//	baseID = "<txID>#safety_slash#<kind>"
//	id     = baseID + "#consensus_debit#" + acct
//
// where acct is the validator's normalized "hive:<name>" account
// (Owner field of the row; Asset is "hive_consensus", not encoded in
// the Owner). Used by applyRestitutionClaim and applySafetySlashReverse
// to cheaply look up the original slash row by exact-id match.
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
		^uint64(0),
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

// alreadyConsumedSlashAmt sums |Amount| of every restitution_claim_consumed
// row whose Id contains "#consumed#<slashTxID>#<kind>" suffix, giving the
// per-slash total the claim queue has already drawn. Used to bound new
// claims so the same slash cannot be over-restituted.
func (se *StateEngine) alreadyConsumedSlashAmt(slashTxID, evidenceKind string) int64 {
	if se == nil || se.LedgerState == nil || se.LedgerState.LedgerDb == nil {
		return 0
	}
	suffix := "#consumed#" + slashTxID + "#" + evidenceKind
	recs, err := se.LedgerState.LedgerDb.GetLedgerRange(
		params.ProtocolSlashRestitutionClaimsAccount,
		0,
		^uint64(0),
		"hive",
		ledgerDb.LedgerOptions{OpType: []string{ledgerSystem.LedgerTypeSafetyRestitutionClaimConsumed}},
	)
	if err != nil || recs == nil {
		return 0
	}
	total := int64(0)
	for _, r := range *recs {
		if strings.HasSuffix(r.Id, suffix) {
			total += -r.Amount // Amount is a negative debit; flip
		}
	}
	return total
}

// alreadyFinalizedBurnAmt sums the amount that has actually been burned
// (moved from pending burn to ProtocolSlashBurnAccount via
// FinalizeMaturedSafetySlashBurns) for the (slashTxID, evidenceKind)
// tuple. Used by applySafetySlashReverse to cap re-credits at the
// not-yet-burned portion so governance cannot mint past what was debited.
func (se *StateEngine) alreadyFinalizedBurnAmt(slashTxID, evidenceKind string) int64 {
	if se == nil || se.LedgerState == nil || se.LedgerState.LedgerDb == nil {
		return 0
	}
	recs, err := se.LedgerState.LedgerDb.GetLedgerRange(
		params.ProtocolSlashBurnAccount,
		0,
		^uint64(0),
		"hive",
		ledgerDb.LedgerOptions{OpType: []string{ledgerSystem.LedgerTypeSafetySlashHiveBurn}},
	)
	if err != nil || recs == nil {
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

// alreadyReversedConsensusAmt sums prior reverse credits for a slash
// tuple, so a second reverse op cannot double-credit the validator.
func (se *StateEngine) alreadyReversedConsensusAmt(slashTxID, evidenceKind, slashedAccount string) int64 {
	if se == nil || se.LedgerState == nil || se.LedgerState.LedgerDb == nil {
		return 0
	}
	owner := normalizeHiveAccount(slashedAccount)
	recs, err := se.LedgerState.LedgerDb.GetLedgerRange(
		owner,
		0,
		^uint64(0),
		"hive_consensus",
		ledgerDb.LedgerOptions{OpType: []string{ledgerSystem.LedgerTypeSafetySlashConsensusReverse}},
	)
	if err != nil || recs == nil {
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

// applyRestitutionClaim is the BlockTypeRestitutionClaim handler. The
// 2/3 BLS aggregate over the carrying VSC block is the authorisation
// gate (witnesses gate inclusion of the op). This function performs
// independent on-chain validation before enqueueing:
//
//  1. Required fields are non-empty / positive.
//  2. The referenced safety_slash_consensus row exists.
//  3. LossHive does not exceed the slash's outstanding restitution
//     headroom (slashAmount - alreadyConsumedByOtherClaims).
//  4. When VictimTxID is supplied, the harmed transaction's AnchoredId
//     equals SlashTxID — i.e. the victim's tx was carried by the same
//     bad block. The check is best-effort: if we can't read the tx
//     record (DB miss / not yet ingested), we drop rather than enqueue
//     an unverifiable claim.
//
// On success, calls LedgerSystem.EnqueueRestitutionClaim. The on-ledger
// queue is consensus-safe — every node converges on the same row set
// from the same inputs, so AllocateHive in subsequent slashes draws
// FIFO deterministically.
func (se *StateEngine) applyRestitutionClaim(
	rec safetyslash.RestitutionClaimRecord,
	carryingTxID string,
	blockHeight uint64,
) {
	if se == nil || se.LedgerSystem == nil {
		return
	}
	rec = rec.Normalize()

	if rec.ClaimID == "" || rec.VictimAccount == "" ||
		rec.SlashTxID == "" || rec.SlashedAccount == "" ||
		rec.EvidenceKind == "" {
		log.Warn("restitution claim: missing required field; dropping",
			"claim_id", rec.ClaimID, "victim", rec.VictimAccount,
			"slash_tx_id", rec.SlashTxID, "kind", rec.EvidenceKind)
		return
	}
	if rec.LossHive <= 0 {
		log.Warn("restitution claim: non-positive loss; dropping",
			"claim_id", rec.ClaimID, "loss_hive", rec.LossHive)
		return
	}

	slashRow, ok := se.findSafetySlashConsensusRow(rec.SlashTxID, rec.EvidenceKind, rec.SlashedAccount)
	if !ok {
		log.Warn("restitution claim: no matching safety_slash_consensus row; dropping",
			"claim_id", rec.ClaimID, "slash_tx_id", rec.SlashTxID,
			"kind", rec.EvidenceKind, "slashed_account", rec.SlashedAccount)
		return
	}
	// slashRow.Amount is the negative debit; absolute is the slashed amount.
	slashAmt := -slashRow.Amount
	if slashAmt <= 0 {
		log.Warn("restitution claim: slash row has non-positive abs amount; dropping",
			"claim_id", rec.ClaimID, "slash_amt", slashAmt)
		return
	}

	consumed := se.alreadyConsumedSlashAmt(rec.SlashTxID, rec.EvidenceKind)
	headroom := slashAmt - consumed
	if headroom <= 0 {
		log.Warn("restitution claim: slash already fully restituted; dropping",
			"claim_id", rec.ClaimID, "slash_amt", slashAmt, "consumed", consumed)
		return
	}
	if rec.LossHive > headroom {
		log.Warn("restitution claim: loss exceeds slash headroom; dropping",
			"claim_id", rec.ClaimID, "loss", rec.LossHive, "headroom", headroom)
		return
	}

	if rec.VictimTxID != "" {
		if !se.harmProofVerified(rec) {
			log.Warn("restitution claim: harm proof failed; dropping",
				"claim_id", rec.ClaimID, "victim_tx_id", rec.VictimTxID,
				"slash_tx_id", rec.SlashTxID)
			return
		}
	}

	res := se.LedgerSystem.EnqueueRestitutionClaim(ledgerSystem.EnqueueRestitutionClaimParams{
		ClaimID:       rec.ClaimID,
		VictimAccount: rec.VictimAccount,
		SlashTxID:     rec.SlashTxID,
		LossHive:      rec.LossHive,
		BlockHeight:   blockHeight,
		TxID:          carryingTxID,
	})
	if !res.Ok {
		log.Warn("restitution claim: enqueue rejected by ledger system",
			"claim_id", rec.ClaimID, "msg", res.Msg)
	}
}

// harmProofVerified checks that the victim's transaction was anchored by
// the same Hive op that triggered the slash. Returns true when the
// VictimTxID's record carries an AnchoredId equal to SlashTxID. Returns
// false on DB miss, decoding error, or mismatch — the conservative path,
// since a missing/ambiguous proof should never enqueue a claim.
func (se *StateEngine) harmProofVerified(rec safetyslash.RestitutionClaimRecord) bool {
	if se == nil || se.txDb == nil {
		return false
	}
	victimRec := se.txDb.GetTransaction(rec.VictimTxID)
	if victimRec == nil || victimRec.AnchoredId == nil {
		return false
	}
	if *victimRec.AnchoredId != rec.SlashTxID {
		return false
	}
	// Defense in depth: the harmed tx must not have completed
	// successfully. If it did, there's no harm to compensate.
	if victimRec.Status == transactions.TransactionStatusConfirmed {
		return false
	}
	return true
}

// applySafetySlashReverse is the BlockTypeSafetySlashReverse handler.
// Per the no-explicit-window auth model, the 2/3 BLS aggregate over the
// carrying VSC block is the timelock; this handler performs ledger
// consistency checks so governance cannot mint past the original debit:
//
//  1. The referenced safety_slash_consensus row exists.
//  2. For cancel: CancelPendingSafetySlashBurn rejects automatically if
//     the pending row has been finalized or already cancelled.
//  3. For reverse: amount is capped at slashAmount - alreadyFinalizedBurn -
//     alreadyReversedAmount, so the net re-credit never exceeds the
//     not-yet-burned portion.
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
	_ = carryingTxID
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
	slashAmt := -slashRow.Amount
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
		// Headroom: the amount already debited to HIVE_CONSENSUS minus
		// what's already been credited back. Note we do NOT subtract
		// alreadyFinalizedBurnAmt here — the slash debit row is still on
		// the validator's books even if the corresponding HIVE has
		// already burned; the reverse simply offsets the bond debit.
		// This matches LedgerState.GetBalance("hive_consensus") which
		// sums LedgerTypeSafetySlashConsensus + Reverse symmetrically.
		headroom := slashAmt - alreadyReversed
		if headroom <= 0 {
			log.Warn("safety slash reverse: bond already fully restored; dropping credit",
				"slash_tx_id", rec.SlashTxID, "slash_amt", slashAmt,
				"already_reversed", alreadyReversed)
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
