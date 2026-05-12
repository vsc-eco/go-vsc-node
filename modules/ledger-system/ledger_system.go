package ledgerSystem

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"vsc-node/lib/vsclog"
	"vsc-node/modules/common"
	"vsc-node/modules/common/params"
	ledger_db "vsc-node/modules/db/vsc/ledger"
)

//Implementation notes:
//The ledger system should operate using a real oplog and virtual ledger
//For example, a deposit record has no oplog, but a virtual ledger op indicating deposit success
//Where as a transfer has an oplog signaling intent, which can be parsed into correct ledger ops to update balance.

// Transaction execution process:
// - Inclusion (hive or vsc)
// - Execution which produces (oplog)
// - Ledger update (virtual ledger)
// In summary:
// 1. TX execution -> 2. Oplog -> 3. Ledger update (locally calculated value)

var log = vsclog.Module("ledger")

type ledgerSystem struct {
	BalanceDb ledger_db.Balances
	LedgerDb  ledger_db.Ledger
	ClaimDb   ledger_db.InterestClaims

	//Bridge actions are side effects that require secondary (outside of execution) processing to complete
	//Some examples are withdrawals, and stake/unstake operations. Other future operations might be applicable as well
	//Anything that requires on chain processing to complete
	ActionsDb ledger_db.BridgeActions
}

const (
	// PendulumNodesHBDBucket is the single global HBD-only bucket holding the
	// cumulative node-runner share since the last epoch close. The swap-time
	// SDK method credits it; the epoch settlement op drains it. The bucket is
	// stored as the Owner field on each LedgerRecord; the asset column ("hbd")
	// is queried separately, so the constant stays asset-suffix-free.
	PendulumNodesHBDBucket = "pendulum:nodes"
)

func (ls *ledgerSystem) PendulumDistribute(toAccount string, amount int64, txID string, blockHeight uint64) LedgerResult {
	if strings.TrimSpace(toAccount) == "" {
		return LedgerResult{Ok: false, Msg: "invalid destination"}
	}
	if amount <= 0 {
		return LedgerResult{Ok: false, Msg: "invalid amount"}
	}
	if txID == "" {
		return LedgerResult{Ok: false, Msg: "missing tx id"}
	}
	available := ls.PendulumBucketBalance(PendulumNodesHBDBucket, blockHeight)
	if available < amount {
		return LedgerResult{Ok: false, Msg: "insufficient pendulum nodes bucket balance"}
	}
	ls.LedgerDb.StoreLedger(
		ledger_db.LedgerRecord{
			Id:          txID + "#distribute_debit#" + toAccount,
			TxId:        txID,
			BlockHeight: blockHeight,
			Amount:      -amount,
			Asset:       "hbd",
			Owner:       PendulumNodesHBDBucket,
			Type:        "pendulum_distribute",
		},
		ledger_db.LedgerRecord{
			Id:          txID + "#distribute_credit#" + toAccount,
			TxId:        txID,
			BlockHeight: blockHeight,
			Amount:      amount,
			Asset:       "hbd",
			Owner:       toAccount,
			Type:        "pendulum_distribute",
		},
	)

	return LedgerResult{Ok: true, Msg: "success"}
}

const (
	// LedgerTypeSafetySlashConsensus debits HIVE_CONSENSUS bond for safety faults.
	LedgerTypeSafetySlashConsensus = "safety_slash_consensus"
	// LedgerTypeSafetySlashRestitution credits liquid HIVE to a harmed party.
	LedgerTypeSafetySlashRestitution = "safety_slash_restitution"
	// LedgerTypeSafetySlashHiveBurn is audit-only; must not count as spendable HIVE.
	LedgerTypeSafetySlashHiveBurn = "safety_slash_hive_burn"
	// LedgerTypeSafetySlashHiveBurnPending holds delayed burn on ProtocolSlashPendingBurnAccount.
	LedgerTypeSafetySlashHiveBurnPending = "safety_slash_hive_burn_pending"
	// LedgerTypeSafetySlashHiveBurnPendingRelease nets out pending when promoting to final burn.
	LedgerTypeSafetySlashHiveBurnPendingRelease = "safety_slash_hive_burn_pending_release"
	// LedgerTypeSafetySlashHiveBurnPendingFinalized marks a pending row as promoted (idempotency).
	LedgerTypeSafetySlashHiveBurnPendingFinalized = "safety_slash_hive_burn_pending_finalized"
	// LedgerTypeSafetySlashHiveBurnPendingCancelled marks a pending row as
	// cancelled before maturity. Paired with LedgerTypeSafetySlashHiveBurnPendingRelease
	// so the pending account nets to 0 and FinalizeMaturedSafetySlashBurns
	// will skip the row even if the cursor revisits it.
	LedgerTypeSafetySlashHiveBurnPendingCancelled = "safety_slash_hive_burn_pending_cancelled"
	// LedgerTypeSafetySlashConsensusReverse re-credits HIVE_CONSENSUS bond
	// previously debited by SafetySlashConsensusBond. Aggregated by
	// LedgerState.GetBalance(hive_consensus). Idempotent per (slashTxID,
	// kind, account) tuple.
	LedgerTypeSafetySlashConsensusReverse = "safety_slash_consensus_reverse"
	// LedgerTypeSafetySlashBurnFinalizeCursor is a single-row meta marker
	// (Owner=ProtocolSlashFinalizeCursorAccount, Amount=0, From=cursor height
	// as a decimal string) used by FinalizeMaturedSafetySlashBurns to bound
	// its scan window. Replaced (upserted by Id) every tick the cursor moves.
	LedgerTypeSafetySlashBurnFinalizeCursor = "safety_slash_burn_finalize_cursor"

	// LedgerTypeSafetyRestitutionClaim is the on-ledger FIFO queue entry for
	// victim restitution claims. Owner=ProtocolSlashRestitutionClaimsAccount,
	// asset=hive, Amount=remaining claim balance (positive = unconsumed),
	// From=normalized victim account. Idempotent per ClaimID via deterministic
	// row Id.
	LedgerTypeSafetyRestitutionClaim = "safety_restitution_claim"

	// LedgerTypeSafetyRestitutionClaimConsumed marks how much of a claim was
	// drawn down by a particular slash. Owner=ProtocolSlashRestitutionClaimsAccount,
	// Amount=signed-debit of remaining claim balance. Replays converge via
	// deterministic id (claim row id + "#consumed#" + slashTxID + "#" + kind).
	LedgerTypeSafetyRestitutionClaimConsumed = "safety_restitution_claim_consumed"

	// safetySlashFinalizeCursorRowID is the deterministic Id used for the
	// single cursor row. Repeating writes upsert the same row.
	safetySlashFinalizeCursorRowID = "safety_slash_burn_finalize_cursor"
)

func normalizeHiveConsensusAccount(a string) string {
	a = strings.TrimSpace(a)
	if a == "" {
		return ""
	}
	if strings.HasPrefix(a, "hive:") {
		return a
	}
	return "hive:" + a
}

// SafetySlashConsensusBond removes up to SlashBps/10000 of the account's current
// HIVE_CONSENSUS bond. The same amount is split between optional FIFO restitution
// (liquid HIVE to victims) and protocol burn on params.ProtocolSlashBurnAccount.
// Deterministic ledger ids make replay idempotent.
func (ls *ledgerSystem) SafetySlashConsensusBond(p SafetySlashConsensusParams) LedgerResult {
	acct := normalizeHiveConsensusAccount(p.Account)
	if acct == "" || strings.TrimSpace(p.TxID) == "" || strings.TrimSpace(p.EvidenceKind) == "" {
		return LedgerResult{Ok: false, Msg: "invalid safety slash params"}
	}
	if p.SlashBps <= 0 || p.SlashBps > 10000 {
		return LedgerResult{Ok: false, Msg: "invalid slash bps"}
	}
	if ls.BalanceDb == nil || ls.LedgerDb == nil {
		return LedgerResult{Ok: false, Msg: "ledger not configured"}
	}

	bondRecord := func(height uint64) *ledger_db.BalanceRecord {
		rec, _ := ls.BalanceDb.GetBalanceRecord(acct, height)
		return rec
	}
	rec := bondRecord(p.BlockHeight)
	if rec == nil || rec.HIVE_CONSENSUS <= 0 {
		if p.BlockHeight > 0 {
			rec = bondRecord(p.BlockHeight - 1)
		}
	}
	if rec == nil || rec.HIVE_CONSENSUS <= 0 {
		return LedgerResult{Ok: false, Msg: "zero consensus bond"}
	}
	bond := rec.HIVE_CONSENSUS

	slashAmt := (bond * int64(p.SlashBps)) / 10000
	if slashAmt <= 0 {
		return LedgerResult{Ok: false, Msg: "slash rounds to zero"}
	}
	if slashAmt > bond {
		slashAmt = bond
	}

	payments, burnAmt := ([]SlashRestitutionPayment)(nil), slashAmt
	if p.Restitution != nil {
		payments, burnAmt = p.Restitution.AllocateHive(slashAmt, p.BlockHeight, p.TxID, p.EvidenceKind, acct)
	}
	var paid int64
	for _, pay := range payments {
		if pay.Amount <= 0 {
			return LedgerResult{Ok: false, Msg: "invalid restitution amount"}
		}
		victim := normalizeHiveConsensusAccount(pay.VictimAccount)
		if victim == "" {
			return LedgerResult{Ok: false, Msg: "invalid restitution victim"}
		}
		if pay.Amount > math.MaxInt64-paid {
			return LedgerResult{Ok: false, Msg: "restitution sum overflow"}
		}
		paid += pay.Amount
	}
	if burnAmt < 0 {
		return LedgerResult{Ok: false, Msg: "invalid slash burn amount"}
	}
	if paid+burnAmt != slashAmt {
		return LedgerResult{Ok: false, Msg: "slash allocation does not reconcile"}
	}

	baseID := p.TxID + "#safety_slash#" + p.EvidenceKind
	records := []ledger_db.LedgerRecord{
		{
			Id:          baseID + "#consensus_debit#" + acct,
			TxId:        p.TxID,
			BlockHeight: p.BlockHeight,
			Amount:      -slashAmt,
			Asset:       "hive_consensus",
			Owner:       acct,
			Type:        LedgerTypeSafetySlashConsensus,
		},
	}
	for i, pay := range payments {
		victim := normalizeHiveConsensusAccount(pay.VictimAccount)
		idSuffix := strings.TrimSpace(pay.ClaimID)
		if idSuffix == "" {
			idSuffix = "_"
		}
		// Payment index is always included so duplicate ClaimID+victim in one slash cannot collide.
		records = append(records, ledger_db.LedgerRecord{
			Id:          baseID + "#restitution#i" + strconv.Itoa(i) + "#" + idSuffix + "#" + victim,
			TxId:        p.TxID,
			BlockHeight: p.BlockHeight,
			Amount:      pay.Amount,
			Asset:       "hive",
			Owner:       victim,
			Type:        LedgerTypeSafetySlashRestitution,
		})
	}
	if burnAmt > 0 {
		if p.BurnDelayBlocks == 0 {
			records = append(records, ledger_db.LedgerRecord{
				Id:          baseID + "#hive_burn#" + acct,
				TxId:        p.TxID,
				BlockHeight: p.BlockHeight,
				Amount:      burnAmt,
				Asset:       "hive",
				Owner:       params.ProtocolSlashBurnAccount,
				Type:        LedgerTypeSafetySlashHiveBurn,
			})
		} else {
			delay := p.BurnDelayBlocks
			if delay > params.MaxSafetySlashBurnDelayBlocks {
				delay = params.MaxSafetySlashBurnDelayBlocks
			}
			maturity := p.BlockHeight + delay
			if maturity < p.BlockHeight {
				return LedgerResult{Ok: false, Msg: "slash burn maturity overflow"}
			}
			records = append(records, ledger_db.LedgerRecord{
				Id:          baseID + "#hive_burn_pending#" + acct,
				TxId:        p.TxID,
				BlockHeight: p.BlockHeight,
				Amount:      burnAmt,
				Asset:       "hive",
				Owner:       params.ProtocolSlashPendingBurnAccount,
				From:        strconv.FormatUint(maturity, 10),
				Type:        LedgerTypeSafetySlashHiveBurnPending,
			})
		}
	}
	ls.LedgerDb.StoreLedger(records...)
	return LedgerResult{Ok: true, Msg: "success"}
}

// readSafetySlashFinalizeCursor returns the lowest emission BlockHeight at
// which any pending burn row could still be unfinalized. Zero on first run or
// if the cursor has not been written yet, which makes the caller fall back to
// scanning from height 0 the first time. The cursor is a single ledger row on
// ProtocolSlashFinalizeCursorAccount, upserted by safetySlashFinalizeCursorRowID,
// so reads always return at most one row.
func (ls *ledgerSystem) readSafetySlashFinalizeCursor() uint64 {
	if ls.LedgerDb == nil {
		return 0
	}
	recs, err := ls.LedgerDb.GetLedgerRange(
		params.ProtocolSlashFinalizeCursorAccount,
		0,
		math.MaxUint64,
		"hive",
		ledger_db.LedgerOptions{OpType: []string{LedgerTypeSafetySlashBurnFinalizeCursor}},
	)
	if err != nil || recs == nil || len(*recs) == 0 {
		return 0
	}
	// Single-row upsert; if more than one is observed (shouldn't happen),
	// take the latest by BlockHeight as a defensive fallback.
	var latestHeight uint64
	var latestCursor uint64
	for _, r := range *recs {
		if r.BlockHeight < latestHeight {
			continue
		}
		c, perr := strconv.ParseUint(strings.TrimSpace(r.From), 10, 64)
		if perr != nil {
			continue
		}
		latestHeight = r.BlockHeight
		latestCursor = c
	}
	return latestCursor
}

// writeSafetySlashFinalizeCursor upserts the cursor row to scanFrom. Called
// only when the new value is strictly greater than the current cursor so we
// never go backward and never write a no-op row.
func (ls *ledgerSystem) writeSafetySlashFinalizeCursor(blockHeight, scanFrom uint64) {
	if ls.LedgerDb == nil {
		return
	}
	ls.LedgerDb.StoreLedger(ledger_db.LedgerRecord{
		Id:          safetySlashFinalizeCursorRowID,
		BlockHeight: blockHeight,
		Amount:      0,
		Asset:       "hive",
		Owner:       params.ProtocolSlashFinalizeCursorAccount,
		From:        strconv.FormatUint(scanFrom, 10),
		Type:        LedgerTypeSafetySlashBurnFinalizeCursor,
	})
}

// FinalizeMaturedSafetySlashBurns promotes pending slash burn rows whose maturity
// height (stored in From) is <= blockHeight. Idempotent per pending row.
//
// Bounded scan: instead of scanning [0, blockHeight] every block, this reads a
// persisted cursor (lowest emission height that could still hold an unfinalized
// pending row) and scans only [cursor, blockHeight]. After processing, the
// cursor advances to the minimum emission height of any row that is still
// unfinalized in the window, or to blockHeight+1 if all rows in the window are
// done. Cursor never moves backward, so concurrent finalization paths and
// replays converge to the same final state.
func (ls *ledgerSystem) FinalizeMaturedSafetySlashBurns(blockHeight uint64) {
	if ls.LedgerDb == nil || blockHeight == 0 {
		return
	}
	pendingAcct := params.ProtocolSlashPendingBurnAccount
	cursor := ls.readSafetySlashFinalizeCursor()
	if cursor > blockHeight {
		// Cursor already past current height (e.g. after a long quiet period).
		// Nothing to do; new pending rows arrive at heights >= blockHeight and
		// will be picked up by future calls.
		return
	}
	pendingRecs, err := ls.LedgerDb.GetLedgerRange(pendingAcct, cursor, blockHeight, "hive", ledger_db.LedgerOptions{
		OpType: []string{LedgerTypeSafetySlashHiveBurnPending},
	})
	if err != nil || pendingRecs == nil {
		return
	}
	finalizedRecs, _ := ls.LedgerDb.GetLedgerRange(pendingAcct, cursor, blockHeight, "hive", ledger_db.LedgerOptions{
		OpType: []string{LedgerTypeSafetySlashHiveBurnPendingFinalized},
	})
	done := make(map[string]struct{})
	if finalizedRecs != nil {
		for _, m := range *finalizedRecs {
			if strings.HasSuffix(m.Id, "#finalized_marker") {
				done[strings.TrimSuffix(m.Id, "#finalized_marker")] = struct{}{}
			}
		}
	}
	// minUnfinalized tracks the lowest emission BlockHeight of any row in
	// [cursor, blockHeight] that we observed but could not finalize yet. If
	// none, the cursor advances past blockHeight.
	var minUnfinalized uint64 = math.MaxUint64
	sawAny := false
	for _, rec := range *pendingRecs {
		sawAny = true
		if rec.Amount <= 0 {
			continue
		}
		if _, ok := done[rec.Id]; ok {
			continue
		}
		mat, err := strconv.ParseUint(strings.TrimSpace(rec.From), 10, 64)
		if err != nil {
			continue
		}
		if mat > blockHeight {
			if rec.BlockHeight < minUnfinalized {
				minUnfinalized = rec.BlockHeight
			}
			continue
		}
		ls.LedgerDb.StoreLedger(
			ledger_db.LedgerRecord{
				Id:          rec.Id + "#pending_release",
				TxId:        rec.TxId,
				BlockHeight: blockHeight,
				Amount:      -rec.Amount,
				Asset:       "hive",
				Owner:       pendingAcct,
				From:        rec.From,
				Type:        LedgerTypeSafetySlashHiveBurnPendingRelease,
			},
			ledger_db.LedgerRecord{
				Id:          rec.Id + "#promoted_to_burn",
				TxId:        rec.TxId,
				BlockHeight: blockHeight,
				Amount:      rec.Amount,
				Asset:       "hive",
				Owner:       params.ProtocolSlashBurnAccount,
				Type:        LedgerTypeSafetySlashHiveBurn,
			},
			ledger_db.LedgerRecord{
				Id:          rec.Id + "#finalized_marker",
				TxId:        rec.TxId,
				BlockHeight: blockHeight,
				Amount:      0,
				Asset:       "hive",
				Owner:       pendingAcct,
				From:        rec.From,
				Type:        LedgerTypeSafetySlashHiveBurnPendingFinalized,
			},
		)
	}
	// Compute new cursor. Only advance if it strictly grows.
	var newCursor uint64
	if !sawAny || minUnfinalized == math.MaxUint64 {
		// Either no pending rows existed in the window, or every row found
		// was finalizable. Either way, the next scan can start past
		// blockHeight; new rows enter at heights >= the next call's
		// blockHeight and are within the new window.
		newCursor = blockHeight + 1
	} else {
		newCursor = minUnfinalized
	}
	if newCursor > cursor {
		ls.writeSafetySlashFinalizeCursor(blockHeight, newCursor)
	}
}

// safetySlashBaseID reproduces the deterministic baseID format used by
// SafetySlashConsensusBond ("{txID}#safety_slash#{kind}"). Cancel and
// reverse callers reuse it so Mongo upserts keep the record set self-
// consistent and replays converge.
func safetySlashBaseID(txID, kind string) string {
	return strings.TrimSpace(txID) + "#safety_slash#" + strings.TrimSpace(kind)
}

// CancelPendingSafetySlashBurn writes a release record that nets the pending
// burn slice for (TxID, EvidenceKind) and a cancellation marker. After a
// successful cancel:
//   - the pending account's HIVE balance for this slash returns to 0
//   - FinalizeMaturedSafetySlashBurns will skip the cancelled row even when
//     its maturity height is reached (the cancellation marker matches the
//     finalized-marker key).
//
// Idempotent: repeated calls with the same (TxID, EvidenceKind) succeed
// silently after the first.
func (ls *ledgerSystem) CancelPendingSafetySlashBurn(p CancelPendingSafetySlashBurnParams) LedgerResult {
	tx := strings.TrimSpace(p.TxID)
	kind := strings.TrimSpace(p.EvidenceKind)
	if tx == "" || kind == "" {
		return LedgerResult{Ok: false, Msg: "invalid cancel params"}
	}
	if ls.LedgerDb == nil {
		return LedgerResult{Ok: false, Msg: "ledger not configured"}
	}
	pendingAcct := params.ProtocolSlashPendingBurnAccount
	// Reproduce the id SafetySlashConsensusBond writes; the slashed account
	// is the trailing component (see line ~242 of this file).
	slashedAcct := normalizeHiveConsensusAccount(p.SlashedAccount)
	if slashedAcct == "" {
		return LedgerResult{Ok: false, Msg: "invalid slashed account"}
	}
	pendingID := safetySlashBaseID(tx, kind) + "#hive_burn_pending#" + slashedAcct
	cancelID := pendingID + "#cancelled_marker"
	releaseID := pendingID + "#cancel_release"
	finalizedID := pendingID + "#finalized_marker"

	// Look up pending row to recover amount + maturity height.
	allPending, err := ls.LedgerDb.GetLedgerRange(pendingAcct, 0, p.BlockHeight, "hive", ledger_db.LedgerOptions{
		OpType: []string{LedgerTypeSafetySlashHiveBurnPending},
	})
	if err != nil || allPending == nil {
		return LedgerResult{Ok: false, Msg: "no pending burn rows"}
	}
	var pendingRec *ledger_db.LedgerRecord
	for i, r := range *allPending {
		if r.Id == pendingID {
			pendingRec = &(*allPending)[i]
			break
		}
	}
	if pendingRec == nil {
		return LedgerResult{Ok: false, Msg: "pending burn row not found"}
	}

	// Order matters: cancel always writes BOTH a cancelled-marker and a
	// finalized-marker (the latter blocks future natural maturation). When a
	// follower-up call arrives, we must report "already cancelled" not
	// "already finalized" so callers can distinguish the two outcomes.
	cancelled, _ := ls.LedgerDb.GetLedgerRange(pendingAcct, 0, p.BlockHeight, "hive", ledger_db.LedgerOptions{
		OpType: []string{LedgerTypeSafetySlashHiveBurnPendingCancelled},
	})
	if cancelled != nil {
		for _, m := range *cancelled {
			if m.Id == cancelID {
				return LedgerResult{Ok: true, Msg: "already cancelled"}
			}
		}
	}

	// Otherwise, if a finalized-marker already exists, the row matured
	// naturally before cancel arrived → reject (governance must use
	// ReverseSafetySlashConsensusDebit instead).
	finalized, _ := ls.LedgerDb.GetLedgerRange(pendingAcct, 0, p.BlockHeight, "hive", ledger_db.LedgerOptions{
		OpType: []string{LedgerTypeSafetySlashHiveBurnPendingFinalized},
	})
	if finalized != nil {
		for _, m := range *finalized {
			if m.Id == finalizedID {
				return LedgerResult{Ok: false, Msg: "pending burn already finalized"}
			}
		}
	}

	// Reason is currently unused (LedgerRecord has no memo field).
	// Logged at the call site for explorers; idempotency lives in the ID.
	_ = strings.TrimSpace(p.Reason)
	ls.LedgerDb.StoreLedger(
		ledger_db.LedgerRecord{
			Id:          releaseID,
			TxId:        pendingRec.TxId,
			BlockHeight: p.BlockHeight,
			Amount:      -pendingRec.Amount,
			Asset:       "hive",
			Owner:       pendingAcct,
			From:        pendingRec.From,
			Type:        LedgerTypeSafetySlashHiveBurnPendingRelease,
		},
		// finalized-marker matching id so FinalizeMaturedSafetySlashBurns
		// skips this row even when maturity is reached.
		ledger_db.LedgerRecord{
			Id:          finalizedID,
			TxId:        pendingRec.TxId,
			BlockHeight: p.BlockHeight,
			Amount:      0,
			Asset:       "hive",
			Owner:       pendingAcct,
			From:        pendingRec.From,
			Type:        LedgerTypeSafetySlashHiveBurnPendingFinalized,
		},
		// distinct cancellation marker so explorers (and idempotency checks)
		// can tell cancellations apart from natural maturations.
		ledger_db.LedgerRecord{
			Id:          cancelID,
			TxId:        pendingRec.TxId,
			BlockHeight: p.BlockHeight,
			Amount:      0,
			Asset:       "hive",
			Owner:       pendingAcct,
			From:        pendingRec.From,
			Type:        LedgerTypeSafetySlashHiveBurnPendingCancelled,
		},
	)
	return LedgerResult{Ok: true, Msg: "cancelled"}
}

// ReverseSafetySlashConsensusDebit re-credits HIVE_CONSENSUS bond previously
// debited by SafetySlashConsensusBond. The credit row's deterministic id is
// {txID}#{kind}#{account}#consensus_reverse so a replay/upsert lands on the
// same row instead of double-crediting.
//
// LedgerState.GetBalance(hive_consensus) sums Amount across consensus_stake,
// consensus_unstake, AND safety_slash_consensus_reverse types so the credit
// applies to spendable bond after this call returns.
func (ls *ledgerSystem) ReverseSafetySlashConsensusDebit(p ReverseSafetySlashConsensusDebitParams) LedgerResult {
	acct := normalizeHiveConsensusAccount(p.Account)
	tx := strings.TrimSpace(p.TxID)
	kind := strings.TrimSpace(p.EvidenceKind)
	if acct == "" || tx == "" || kind == "" {
		return LedgerResult{Ok: false, Msg: "invalid reverse params"}
	}
	if p.Amount <= 0 {
		return LedgerResult{Ok: false, Msg: "reverse amount must be positive"}
	}
	if ls.LedgerDb == nil {
		return LedgerResult{Ok: false, Msg: "ledger not configured"}
	}
	_ = strings.TrimSpace(p.Reason)
	// Include OpInstanceID in the row Id so two distinct chain-op
	// reverses on the same (tx, kind, acct) accumulate rather than
	// upserting onto a single shared row. Replays of the same chain op
	// pass the same OpInstanceID and converge via Mongo upsert.
	id := safetySlashBaseID(tx, kind) + "#consensus_reverse#" + acct
	if op := strings.TrimSpace(p.OpInstanceID); op != "" {
		id = id + "#" + op
	}
	ls.LedgerDb.StoreLedger(ledger_db.LedgerRecord{
		Id:          id,
		TxId:        tx,
		BlockHeight: p.BlockHeight,
		Amount:      p.Amount,
		Asset:       "hive_consensus",
		Owner:       acct,
		Type:        LedgerTypeSafetySlashConsensusReverse,
	})
	return LedgerResult{Ok: true, Msg: "reverse credit recorded"}
}

// restitutionClaimRowID returns the deterministic Id for a claim row. Used
// by both EnqueueRestitutionClaim (write) and OnLedgerRestitutionAllocator
// (read + consume marker derivation) so replays converge.
func restitutionClaimRowID(claimID string) string {
	return "safety_restitution_claim#" + strings.TrimSpace(claimID)
}

// EnqueueRestitutionClaim writes a restitution claim row on
// params.ProtocolSlashRestitutionClaimsAccount. Idempotent per ClaimID:
// re-enqueueing the same claim upserts the same row, which is critical for
// chain replay since the carrying block-content tx may be observed multiple
// times during catch-up.
func (ls *ledgerSystem) EnqueueRestitutionClaim(p EnqueueRestitutionClaimParams) LedgerResult {
	if ls.LedgerDb == nil {
		return LedgerResult{Ok: false, Msg: "ledger not configured"}
	}
	cid := strings.TrimSpace(p.ClaimID)
	victim := normalizeHiveAcct(p.VictimAccount)
	if cid == "" || victim == "" {
		return LedgerResult{Ok: false, Msg: "claim id and victim account are required"}
	}
	if p.LossHive <= 0 {
		return LedgerResult{Ok: false, Msg: "claim loss must be positive"}
	}
	tx := strings.TrimSpace(p.TxID)
	if tx == "" {
		// TxID is forwarded verbatim into the row's TxId field for
		// explorer attribution; if missing, fall back to the ClaimID.
		tx = cid
	}
	ls.LedgerDb.StoreLedger(ledger_db.LedgerRecord{
		Id:          restitutionClaimRowID(cid),
		TxId:        tx,
		BlockHeight: p.BlockHeight,
		Amount:      p.LossHive,
		Asset:       "hive",
		Owner:       params.ProtocolSlashRestitutionClaimsAccount,
		From:        victim,
		Type:        LedgerTypeSafetyRestitutionClaim,
	})
	return LedgerResult{Ok: true, Msg: "claim enqueued"}
}

// normalizeHiveAcct mirrors normalizeHiveAccount in state-processing for
// the ledger-system package boundary. We intentionally keep the function
// local rather than depending on state-processing to avoid an import cycle.
func normalizeHiveAcct(s string) string {
	v := strings.TrimSpace(s)
	if v == "" {
		return ""
	}
	if strings.HasPrefix(v, "hive:") {
		return v
	}
	return "hive:" + v
}

// PendulumBucketBalance sums every ledger record whose Owner == bucket and
// Asset == hbd. The bucket is credited via paired LedgerSession.ExecuteTransfer
// records (type=transfer) at swap time and debited via PendulumDistribute
// records (type=pendulum_distribute) at settlement time.
func (ls *ledgerSystem) PendulumBucketBalance(bucket string, blockHeight uint64) int64 {
	if strings.TrimSpace(bucket) == "" {
		return 0
	}
	records, err := ls.LedgerDb.GetLedgerRange(bucket, 0, blockHeight, "hbd")
	if err != nil || records == nil {
		return 0
	}
	total := int64(0)
	for _, rec := range *records {
		total += rec.Amount
	}
	return total
}

func (ls *ledgerSystem) ClaimHBDInterest(lastClaim uint64, blockHeight uint64, amount int64, txId string) {
	fmt.Println("ClaimHBDInterest", lastClaim, blockHeight, amount)
	//Do distribution of HBD interest on an going forward basis
	//Save to ledger DB the difference.
	ledgerBalances := ls.BalanceDb.GetAll(blockHeight)

	processedBalRecords := make([]ledger_db.BalanceRecord, 0)
	totalAvg := int64(0)
	//Ensure averages have been updated before distribution;
	for _, balance := range ledgerBalances {

		if blockHeight <= balance.HBD_CLAIM_HEIGHT {
			continue // Avoid underflow when claim height is at or beyond current block
		}
		B := blockHeight - balance.HBD_CLAIM_HEIGHT //Total blocks since last claim
		if B == 0 {
			continue //Avoid division by zero when claim height equals block height
		}
		if blockHeight <= balance.HBD_MODIFY_HEIGHT {
			continue // Avoid underflow when modify height is at or beyond current block
		}
		A := blockHeight - balance.HBD_MODIFY_HEIGHT //Blocks since last balance modification

		//HBD_AVG is an unnormalized cumulative sum (balance * blocks).
		//Add the current balance's contribution for the remaining period, then divide by B to get TWAB.
		endingAvg := (balance.HBD_AVG + balance.HBD_SAVINGS*int64(A)) / int64(B)

		if endingAvg < 1 {
			fmt.Println("ClaimHBD endingAvg is sub zero", balance.Account, endingAvg)
			continue
		}

		balance.HBD_AVG = endingAvg

		processedBalRecords = append(processedBalRecords, balance)
		totalAvg = totalAvg + endingAvg
	}

	bsj, _ := json.Marshal(processedBalRecords)

	fmt.Println("Processed bal records", ledgerBalances, string(bsj))

	for id, balance := range processedBalRecords {
		// if balance.HBD_AVG == 0 {
		// 	continue
		// }

		distributeAmt := balance.HBD_AVG * amount / totalAvg

		if distributeAmt > 0 {
			var owner string
			if strings.HasPrefix(balance.Account, "system:") {
				if balance.Account == "system:fr_balance" {
					owner = params.DAO_WALLET
				} else {
					//Filter
					continue
				}
			} else {
				owner = balance.Account
			}

			ls.LedgerDb.StoreLedger(ledger_db.LedgerRecord{
				Id: "hbd_interest_" + strconv.Itoa(int(blockHeight)) + "_" + strconv.Itoa(id),
				//next block
				BlockHeight: blockHeight + 1,
				Amount:      int64(distributeAmt),
				Asset:       "hbd_savings",
				Owner:       owner,
				Type:        "interest",
			})
		}
	}
	//Note this calculation is inaccurate and should be calculated based on N blocks of Y claim period
	//Should not assume a static amount of time or 12 exactly claim interals per year. But since this is for statistics, it doesn't matter.
	var observedApr float64
	if totalAvg > 0 {
		observedApr = (float64(amount) / float64(totalAvg)) * 12
	}

	savedAmount := amount
	if totalAvg == 0 {
		savedAmount = 0
	}
	ls.ClaimDb.SaveClaim(ledger_db.ClaimRecord{
		BlockHeight: blockHeight,
		Amount:      savedAmount,
		TxId:        txId,
		ReceivedN:   len(processedBalRecords),
		ObservedApr: observedApr,
	})
	//DONE
}

func (ls *ledgerSystem) IndexActions(actionUpdate map[string]interface{}, extraInfo ExtraInfo) {
	log.Debug("IndexActions", "actionUpdate", actionUpdate)

	actionIds := common.ArrayToStringArray(actionUpdate["ops"].([]interface{}))

	//All stake related ops
	completeOps := actionUpdate["cleared_ops"].(string)

	b64, _ := base64.RawURLEncoding.DecodeString(completeOps)

	bs := big.Int{}

	bs.SetBytes(b64)

	for idx, id := range actionIds {
		record, _ := ls.ActionsDb.Get(id)
		if record == nil {
			continue
		}
		ls.ActionsDb.ExecuteComplete(&extraInfo.ActionId, id)

		if record.Type == "stake" {
			log.Debug("Indexxing stake Ledger")
			ls.LedgerDb.StoreLedger(ledger_db.LedgerRecord{
				Id:     record.Id + "#out",
				Amount: record.Amount,
				Asset:  "hbd_savings",
				Owner:  record.To,
				Type:   "stake",

				//Next block balance should be clear
				BlockHeight: extraInfo.BlockHeight + 1,
				//Before everything
				BIdx:  -1,
				OpIdx: -1,
			})
		}
		if record.Type == "unstake" {
			var blockDelay uint64

			//If is a neutral stake op, then set only 1 block delay
			if bs.Bit(idx) == 1 {
				blockDelay = 1
			} else {
				blockDelay = common.HBD_UNSTAKE_BLOCKS
			}
			ls.LedgerDb.StoreLedger(ledger_db.LedgerRecord{
				Id:     record.Id + "#out",
				Amount: record.Amount,
				Asset:  "hbd",
				Owner:  record.To,
				Type:   "unstake",

				//It'll become available in 3 days of blocks
				BlockHeight: extraInfo.BlockHeight + blockDelay,
				BIdx:        -1,
				OpIdx:       -1,
			})
		}
	}

}

func (ls *ledgerSystem) IngestOplog(oplog []OpLogEvent, options OplogInjestOptions) {
	executeResults := ExecuteOplog(oplog, options.StartHeight, options.EndHeight)

	ledgerRecords := executeResults.ledgerRecords
	actionRecords := executeResults.actionRecords

	for _, v := range ledgerRecords {
		ls.LedgerDb.StoreLedger(ledger_db.LedgerRecord{
			Id: v.Id,
			//plz passthrough original block height
			BlockHeight: options.EndHeight,
			Amount:      v.Amount,
			Asset:       v.Asset,
			Owner:       v.Owner,
			Type:        v.Type,
			// TxId:        v.,
		})
	}

	for _, v := range actionRecords {
		ls.ActionsDb.StoreAction(v)
	}

}

func (ls *ledgerSystem) GetBalance(account string, blockHeight uint64, asset string) int64 {
	lsz := ls.NewEmptyState()

	return lsz.GetBalance(account, blockHeight, asset)
}

// Used during live execution of transfers such as those from contracts or user transaction

// Empties the virtual state, such as when a block is executed

func (ls *ledgerSystem) Deposit(deposit Deposit) string {
	decodedParams := DepositParams{}
	values, err := url.ParseQuery(deposit.Memo)
	if err == nil {
		decodedParams.To = values.Get("to")
		// fmt.Println("decodedParams.To", decodedParams.To)
	} else {
		err = json.Unmarshal([]byte(deposit.Memo), &decodedParams)
		if err != nil {
			decodedParams.To = deposit.From
		}
	}

	matchedHive, _ := regexp.MatchString(HIVE_REGEX, decodedParams.To)
	matchedEth, _ := regexp.MatchString(ETH_REGEX, decodedParams.To)

	if matchedHive && len(decodedParams.To) >= 3 && len(decodedParams.To) < 17 {
		decodedParams.To = `hive:` + decodedParams.To
	} else if matchedEth {
		decodedParams.To = `did:pkh:eip155:1:` + decodedParams.To
	} else if strings.HasPrefix(decodedParams.To, "hive:") {
		//No nothing. It's parsed correctly
		matchedEth, _ := regexp.MatchString(HIVE_REGEX, strings.Split(decodedParams.To, ":")[1])
		if !(matchedEth && len(decodedParams.To) >= 3 && len(decodedParams.To) < 17) {
			decodedParams.To = "hive:" + deposit.From
		}
	} else if strings.HasPrefix(decodedParams.To, "did:") {
		//No nothing. It's parsed correctly
	} else {
		//Default to the original sender to prevent fund loss
		// addr, _ := NormalizeAddress(deposit.From, "hive")
		decodedParams.To = deposit.From
	}
	// if le.VirtualLedger == nil {
	// 	le.VirtualLedger = make(map[string][]LedgerUpdate)
	// }

	// if le.VirtualLedger[decodedParams.To] != nil {
	// 	le.Ls.log.Debug("ledgerExecutor", le.VirtualLedger[decodedParams.To])
	// }

	ls.LedgerDb.StoreLedger(ledger_db.LedgerRecord{
		Id:          deposit.Id,
		BlockHeight: deposit.BlockHeight,
		Amount:      deposit.Amount,
		Asset:       deposit.Asset,
		From:        deposit.From,
		Owner:       decodedParams.To,
		Type:        "deposit",
		TxId:        deposit.Id,
	})

	return decodedParams.To
}

func (ls *ledgerSystem) CalculationFractStats(accountList []string, blockHeight uint64) struct {
	//Total staked balance of hbd_savings accounts
	StakedBalance int64

	//Total fractional balance; must be staked
	FractionalBalance int64

	//Both numbers should add together to be equivalent to the total hbd savings of the vsc gateway wallet
} {

	//TODO: Make this work without needing to supply list of accounts.
	//Instead pull all accounts above >0 balance

	state := ls.NewEmptyState()

	hbdMap := make(map[string]int64)
	hbdSavingsMap := make(map[string]int64)

	for _, account := range accountList {
		hbdBal := state.SnapshotForAccount(account, blockHeight, "hbd")
		hbdSavingsBal := state.SnapshotForAccount(account, blockHeight, "hbd_savings")
		hbdMap[account] = hbdBal
		hbdSavingsMap[account] = hbdSavingsBal
	}

	topBalances := make([]int64, 0)

	for _, v := range hbdMap {
		topBalances = append(topBalances, v)
	}
	sort.Slice(topBalances, func(i, j int) bool {
		return topBalances[i] > topBalances[j]
	})

	cutOff := 3
	topBals := topBalances[:cutOff]
	belowBals := topBalances[cutOff:]

	topBal := int64(0)
	belowBal := int64(0)

	for _, v := range topBals {
		topBal = topBal + v
	}

	for _, v := range belowBals {
		belowBal = belowBal + v
	}

	fmt.Println("Top Balances", topBalances)
	fmt.Println("Top 5", topBal, "Below 5", belowBal)
	stakedAmt := belowBal / 3
	fmt.Println("StakedAmt", stakedAmt)

	StakedBalance := int64(0)

	for _, v := range hbdSavingsMap {
		StakedBalance = StakedBalance + v
	}

	return struct {
		StakedBalance     int64
		FractionalBalance int64
	}{
		StakedBalance:     StakedBalance,
		FractionalBalance: stakedAmt,
	}
}

func (ls *ledgerSystem) NewEmptySession(state *LedgerState, startHeight uint64) LedgerSession {
	ledgerSession := ledgerSession{
		state: state,

		oplog:     make([]OpLogEvent, 0),
		ledgerOps: make([]LedgerUpdate, 0),
		balances:  make(map[string]*int64),
		idCache:   make(map[string]int),

		StartHeight: startHeight,
	}
	return &ledgerSession
}

func (ls *ledgerSystem) NewEmptyState() *LedgerState {
	return &LedgerState{
		Oplog:           make([]OpLogEvent, 0),
		VirtualLedger:   make(map[string][]LedgerUpdate),
		GatewayBalances: make(map[string]uint64),
		LedgerDb:        ls.LedgerDb,
		ActionDb:        ls.ActionsDb,
		BalanceDb:       ls.BalanceDb,
	}
}

// HBD instant stake/unstake fee

// Reserved for the future.
// Stake would trigger an indent to stake funds (immediately removing balance)
// Then trigger a delayed (actual stake) even when the onchain operation is executed through the gateway
// A two part Virtual Ledger operation operating out of sync

func New(balanceDb ledger_db.Balances, ledgerDb ledger_db.Ledger, claimDb ledger_db.InterestClaims, actionDb ledger_db.BridgeActions) LedgerSystem {
	return &ledgerSystem{
		BalanceDb: balanceDb,
		LedgerDb:  ledgerDb,
		ClaimDb:   claimDb,
		ActionsDb: actionDb,
	}
}
