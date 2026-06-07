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
	"time"
	"vsc-node/lib/vsclog"
	"vsc-node/modules/common"
	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"
	ledger_db "vsc-node/modules/db/vsc/ledger"

	ethcommon "github.com/ethereum/go-ethereum/common"
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

// Hive produces one block every 3 seconds → 365.25 * 24 * 3600 / 3 blocks per year.
const HIVE_BLOCKS_PER_YEAR = uint64(10_519_200)

type ledgerSystem struct {
	BalanceDb ledger_db.Balances
	LedgerDb  ledger_db.Ledger
	ClaimDb   ledger_db.InterestClaims

	//Bridge actions are side effects that require secondary (outside of execution) processing to complete
	//Some examples are withdrawals, and stake/unstake operations. Other future operations might be applicable as well
	//Anything that requires on chain processing to complete
	ActionsDb ledger_db.BridgeActions

	// sconf is the network's system config. Per-network activation heights
	// (EvmAddressChecksumHeight, etc.) are read through it
	sconf systemconfig.SystemConfig
}

const (
	// PendulumNodesHBDBucket is the single global HBD-only bucket holding the
	// cumulative node-runner share since the last epoch close. The swap-time
	// SDK method credits it; the epoch settlement op drains it. The bucket is
	// stored as the Owner field on each LedgerRecord; the asset column ("hbd")
	// is queried separately, so the constant stays asset-suffix-free.
	PendulumNodesHBDBucket = "pendulum:nodes"
)

func (ls *ledgerSystem) PendulumDistribute(
	toAccount string,
	amount int64,
	txID string,
	blockHeight uint64,
) LedgerResult {
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
	if err := ls.LedgerDb.StoreLedger(
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
	); err != nil {
		log.Error(
			"PendulumDistribute: ledger write failed",
			"txId",
			txID,
			"account",
			toAccount,
			"amount",
			amount,
			"err",
			err,
		)
		return LedgerResult{Ok: false, Msg: "ledger write failed"}
	}

	return LedgerResult{Ok: true, Msg: "success"}
}

const (
	// LedgerTypeSafetySlashConsensus debits HIVE_CONSENSUS bond for safety faults.
	LedgerTypeSafetySlashConsensus = "safety_slash_consensus"
	// LedgerTypeSafetySlashHiveBurn is audit-only; must not count as spendable HIVE.
	LedgerTypeSafetySlashHiveBurn = "safety_slash_hive_burn"
	// LedgerTypeSafetySlashReserve credits the safety-slash residual to the
	// keyless insurance reserve (ProtocolSlashReserveAccount) INSTEAD of burning
	// it (destination change). The reserve is a keyless, no-owner INSURANCE
	// backstop with no extraction path — value never leaves it; it is NOT part
	// of the V/E collateral computation (the bond is collateral). Audit-only:
	// MUST be excluded from both spendable-HIVE aggregation sites (GetBalance
	// allow-list AND UpdateBalances deny-list switch), exactly like the burn
	// op-type, or the reserve would mint spendable HIVE.
	LedgerTypeSafetySlashReserve = "safety_slash_reserve"
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

	// safetySlashFinalizeCursorRowID is the deterministic Id used for the
	// single cursor row. Repeating writes upsert the same row.
	safetySlashFinalizeCursorRowID = "safety_slash_burn_finalize_cursor"
)

// IsProtocolMetaLedgerType reports whether a ledger record type is a
// protocol-internal meta row that must NOT count toward any spendable balance.
// These are the burn / pending-burn / finalize-cursor / restitution-claim queue
// rows: they live on protocol-owned accounts and represent bookkeeping state,
// never spendable funds on a user's own account.
//
// This is the SINGLE source of truth for that exclusion set. Both
// LedgerState.GetBalance's incremental delta and state_engine.UpdateBalances'
// snapshot fold must use it, so the two can never drift apart — the drift
// between an exclude-list fold and an allow-list delta was the CRIT-1
// double-spend.
func IsProtocolMetaLedgerType(t string) bool {
	switch t {
	case LedgerTypeSafetySlashHiveBurn,
		LedgerTypeSafetySlashHiveBurnPending,
		LedgerTypeSafetySlashHiveBurnPendingRelease,
		LedgerTypeSafetySlashHiveBurnPendingFinalized,
		LedgerTypeSafetySlashHiveBurnPendingCancelled,
		LedgerTypeSafetySlashBurnFinalizeCursor,
		LedgerTypeSafetyRestitutionClaim,
		LedgerTypeSafetyRestitutionClaimConsumed:
		return true
	default:
		return false
	}
}

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
// HIVE_CONSENSUS bond. The full slashed amount is the residual, which lands in
// the keyless reserve on params.ProtocolSlashReserveAccount (destination change
// — no longer burned; the reserve is a retained backstop with no extraction
// path). There is no victim payout: the only slashable faults are
// consensus-integrity faults with no fund victim.
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

	// slashAmt = bond·SlashBps/10000, computed overflow-safe. The naive
	// `bond*SlashBps` overflows int64 once bond > MaxInt64/10000 (~9.2e14 sats,
	// far above HIVE supply, so unreachable today) and would wrap negative,
	// silently turning a real slash into a "rounds to zero" no-op (a guilty
	// validator goes unslashed). Splitting the division avoids the large
	// intermediate product — exact integer floor, identical result, no overflow,
	// deterministic. (bond = 10000·q + r ⇒ bond·bps/10000 = q·bps + r·bps/10000.)
	slashAmt := (bond/10000)*int64(p.SlashBps) + ((bond%10000)*int64(p.SlashBps))/10000
	if slashAmt <= 0 {
		return LedgerResult{Ok: false, Msg: "slash rounds to zero"}
	}
	if slashAmt > bond {
		slashAmt = bond
	}

	// The full slashed amount is the residual; it is committed to the keyless
	// reserve (immediately, or via the pending challenge window below).
	burnAmt := slashAmt

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
	if burnAmt > 0 {
		if p.BurnDelayBlocks == 0 {
			// Destination change: residual lands in the keyless reserve, not the
			// burn sink. Id keeps the baseID prefix so alreadyFinalizedToReserveAmt
			// (the reverse cap) sees it.
			records = append(records, ledger_db.LedgerRecord{
				Id:          baseID + "#reserve#" + acct,
				TxId:        p.TxID,
				BlockHeight: p.BlockHeight,
				Amount:      burnAmt,
				Asset:       "hive",
				Owner:       params.ProtocolSlashReserveAccount,
				Type:        LedgerTypeSafetySlashReserve,
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
				// Destination change: matured residual is promoted to the keyless
				// reserve instead of the burn sink. rec.Id (the pending row) keeps
				// the baseID prefix so the reverse cap's alreadyFinalizedToReserveAmt
				// prefix match still bounds reversals.
				Id:          rec.Id + "#promoted_to_reserve",
				TxId:        rec.TxId,
				BlockHeight: blockHeight,
				Amount:      rec.Amount,
				Asset:       "hive",
				Owner:       params.ProtocolSlashReserveAccount,
				Type:        LedgerTypeSafetySlashReserve,
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

// blockingRetry retries a consensus-critical DB read until it succeeds, halting
// the deterministic block-processing path on a transient infra error instead of
// swallowing it and diverging from peers. Mirrors the fail-stop helper in
// modules/state-processing (state_engine.go: blockingRetry).
func blockingRetry(what string, read func() error) {
	const (
		baseDelay = 100 * time.Millisecond
		maxDelay  = 30 * time.Second
	)
	delay := baseDelay
	for attempt := 1; ; attempt++ {
		if err := read(); err == nil {
			if attempt > 1 {
				log.Error("DB read recovered; resuming", "op", what, "attempts", attempt)
			}
			return
		} else {
			log.Error("DB read failed; halting until DB recovers (fail-stop)",
				"op", what, "attempt", attempt, "retryIn", delay.String(), "err", err)
		}
		time.Sleep(delay)
		if delay < maxDelay {
			if delay *= 2; delay > maxDelay {
				delay = maxDelay
			}
		}
	}
}

func (ls *ledgerSystem) ClaimHBDInterest(lastClaim uint64, blockHeight uint64, amount int64, txId string) {
	log.Verbose("ClaimHBDInterest", "lastClaim", lastClaim, "bh", blockHeight, "amount", amount)
	//Do distribution of HBD interest on an going forward basis
	//Save to ledger DB the difference.
	//
	// Fail-stop on a GetAll error rather than returning: this runs on the
	// deterministic block-processing path and a returned-early node would skip
	// the entire interest distribution (and SaveClaim) while healthy peers
	// distribute, diverging the ledger — a fork. The error is a transient infra
	// failure (Mongo), not a deterministic absence, so block until it recovers.
	// Mirrors state-processing's blockingRetry / getLedgerRangeOrBlock.
	var ledgerBalances []ledger_db.BalanceRecord
	blockingRetry(fmt.Sprintf("ClaimHBDInterest.GetAll(@%d)", blockHeight), func() error {
		var err error
		ledgerBalances, err = ls.BalanceDb.GetAll(blockHeight)
		return err
	})

	processedBalRecords := make([]ledger_db.BalanceRecord, 0)
	// GV-L2: totalAvg is the cross-account SUM of every account's endingAvg.
	// A single endingAvg fits int64 (computeEndingAvg guards it), but the SUM
	// across all accounts is not int64-bounded. As an int64 accumulator it
	// wrapped negative once the sum crossed math.MaxInt64, and a negative
	// denominator passed straight through computeDistributeAmount's old
	// `== 0` guard, producing negative per-account shares that the `> 0`
	// filter below then dropped — skipping the whole epoch's distribution
	// while SaveClaim still recorded the full amount. Keep the accumulator in
	// arbitrary precision so it can never wrap.
	totalAvgBig := new(big.Int)
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

		// HBD_AVG is an unnormalized cumulative sum (balance * blocks).
		// Add the current balance's contribution for the remaining period, then divide by B to get TWAB.
		// Overflow-safe — see review4 HIGH #15 / computeEndingAvg.
		endingAvg, okAvg := computeEndingAvg(balance.HBD_AVG, balance.HBD_SAVINGS, int64(A), int64(B))
		if !okAvg {
			log.Warn("ClaimHBD endingAvg overflows int64", "account", balance.Account)
			continue
		}

		if endingAvg < 1 {
			log.Verbose("ClaimHBD endingAvg sub-zero", "account", balance.Account, "avg", endingAvg)
			continue
		}

		balance.HBD_AVG = endingAvg

		processedBalRecords = append(processedBalRecords, balance)
		totalAvgBig.Add(totalAvgBig, big.NewInt(endingAvg))
	}

	log.Verbose("processed bal records", "count", len(processedBalRecords), "totalAvg", totalAvgBig.String())

	// GV-L1: computeDistributeAmount uses floor division, so the sum of all
	// per-account shares is amount-(remainder), where remainder can be up to
	// N-1 milli-HBD (N = recipients). The pre-fix code never credited that
	// remainder anywhere yet SaveClaim recorded the full `amount`, so the
	// claim record claimed more was paid out than the ledger actually
	// received — a permanent accounting lie (and the same overstatement
	// happened on the GV-L2 overflow path, where ZERO was credited but
	// `amount` was still recorded).
	//
	// USER DECISION (accounting-only): do NOT redistribute the dust and do
	// NOT write an extra ledger record. Leave the floor-division remainder
	// un-distributed wherever it currently is, and make SaveClaim record the
	// TRUTHFUL amount actually credited (`distributed` = sum of the floor-div
	// shares that were genuinely written to the ledger). This keeps balances
	// byte-for-byte IDENTICAL to pristine (no new credit anywhere) while the
	// claim RECEIPT stops overstating. Track the running total below.
	distributed := int64(0)
	for id, balance := range processedBalRecords {
		// if balance.HBD_AVG == 0 {
		// 	continue
		// }

		// Overflow-safe HBD_AVG * amount / totalAvg — see review4 HIGH #15.
		distributeAmt, okDist := computeDistributeAmount(balance.HBD_AVG, amount, totalAvgBig)
		if !okDist {
			log.Warn("ClaimHBD distributeAmt overflows int64", "account", balance.Account)
			continue
		}

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

			if err := ls.LedgerDb.StoreLedger(ledger_db.LedgerRecord{
				Id: "hbd_interest_" + strconv.Itoa(int(blockHeight)) + "_" + strconv.Itoa(id),
				//next block
				BlockHeight: blockHeight + 1,
				Amount:      int64(distributeAmt),
				Asset:       "hbd_savings",
				Owner:       owner,
				Type:        "interest",
			}); err != nil {
				log.Error(
					"ClaimHBDInterest: ledger write failed",
					"blockHeight",
					blockHeight,
					"owner",
					owner,
					"err",
					err,
				)
				// GV-L1: only count toward `distributed` when the write
				// actually succeeded — a failed write credited nothing, so
				// counting it would re-introduce the overstatement this fix
				// removes.
				continue
			}
			distributed += distributeAmt
		}
	}
	// observedApr is a non-consensus statistic. Keep develop's accurate
	// annualization — the realized rate scaled by HIVE_BLOCKS_PER_YEAR over the
	// ACTUAL blocks in the claim period (not a fixed 12 intervals/year) — but
	// compute it in arbitrary precision (GV-L2): totalAvgBig is the cross-account
	// SUM of every endingAvg and can exceed int64, so a big.Float divide never
	// wraps where the old int64 `totalAvg > 0` compare could fail open on a
	// wrapped-negative accumulator. lastClaim==0 (no prior claim) leaves
	// observedApr at 0 — the first claim has no prior period to annualize over.
	var observedApr float64
	if totalAvgBig.Sign() > 0 && lastClaim > 0 && blockHeight > lastClaim {
		blocksInPeriod := blockHeight - lastClaim
		aprBig := new(big.Float).Quo(new(big.Float).SetInt64(amount), new(big.Float).SetInt(totalAvgBig))
		aprBig.Mul(aprBig, big.NewFloat(float64(HIVE_BLOCKS_PER_YEAR)/float64(blocksInPeriod)))
		observedApr, _ = aprBig.Float64()
	}

	// GV-L1: record what was ACTUALLY distributed (the sum of the floor-div
	// shares genuinely credited), NOT the nominal `amount`. The floor-division
	// dust remains un-distributed and `distributed` is therefore <= `amount`.
	// When no account qualified (or the GV-L2 overflow path skipped every
	// share pre-fix — now prevented), `distributed` is 0, matching the old
	// `totalAvg == 0 → savedAmount = 0` semantics while no longer overstating
	// the claim when distribution was partial or skipped.
	savedAmount := distributed
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

	// review2 MED #86/#88 (sweep): actionUpdate is json.Unmarshal'd
	// from an L1 custom_json payload. Bare assertions on ops /
	// cleared_ops panicked block processing on a malformed (or
	// future-divergent) vsc.actions op. Comma-ok and skip indexing
	// this op deterministically instead.
	opsRaw, opsOk := actionUpdate["ops"].([]interface{})
	completeOps, clearedOk := actionUpdate["cleared_ops"].(string)
	if !opsOk || !clearedOk {
		log.Warn("skipping malformed vsc.actions op (ops/cleared_ops wrong type)",
			"actionId", extraInfo.ActionId, "blockHeight", extraInfo.BlockHeight)
		return
	}

	actionIds := common.ArrayToStringArray(opsRaw)

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
			if err := ls.LedgerDb.StoreLedger(ledger_db.LedgerRecord{
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
			}); err != nil {
				log.Error("IndexActions: stake ledger write failed", "id", record.Id, "to", record.To, "err", err)
			}
		}
		if record.Type == "unstake" {
			var blockDelay uint64

			//If is a neutral stake op, then set only 1 block delay
			if bs.Bit(idx) == 1 {
				blockDelay = 1
			} else {
				blockDelay = common.HBD_UNSTAKE_BLOCKS
			}
			if err := ls.LedgerDb.StoreLedger(ledger_db.LedgerRecord{
				Id:     record.Id + "#out",
				Amount: record.Amount,
				Asset:  "hbd",
				Owner:  record.To,
				Type:   "unstake",

				//It'll become available in 3 days of blocks
				BlockHeight: extraInfo.BlockHeight + blockDelay,
				BIdx:        -1,
				OpIdx:       -1,
			}); err != nil {
				log.Error("IndexActions: unstake ledger write failed", "id", record.Id, "to", record.To, "err", err)
			}
		}
	}

}

func (ls *ledgerSystem) IngestOplog(oplog []OpLogEvent, options OplogInjestOptions) {
	executeResults := ExecuteOplog(oplog, options.StartHeight, options.EndHeight)

	ledgerRecords := executeResults.ledgerRecords
	actionRecords := executeResults.actionRecords

	for _, v := range ledgerRecords {
		if err := ls.LedgerDb.StoreLedger(ledger_db.LedgerRecord{
			Id: v.Id,
			//plz passthrough original block height
			BlockHeight: options.EndHeight,
			Amount:      v.Amount,
			Asset:       v.Asset,
			Owner:       v.Owner,
			Type:        v.Type,
			// TxId:        v.,
		}); err != nil {
			log.Error("IngestOplog: ledger write failed", "id", v.Id, "owner", v.Owner, "err", err)
		}
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

// ethDIDPrefix is the did:pkh prefix for an EVM (eip155 mainnet) account.
const ethDIDPrefix = "did:pkh:eip155:1:"

// normalizeEthDID rewrites a did:pkh:eip155 account to its canonical EIP-55
// checksummed form. Non-eip155 accounts (hive:, did:key:, etc.) and
// malformed addresses are returned unchanged, so it is safe to apply to the
// already-resolved owner regardless of which branch produced it. The address
// portion is regex-validated before HexToAddress so go-ethereum's lenient
// parser can't silently truncate or pad a bad input.
func normalizeEthDID(account string) string {
	addr, ok := strings.CutPrefix(account, ethDIDPrefix)
	if !ok {
		return account
	}
	if matched, _ := regexp.MatchString(ETH_REGEX, addr); !matched {
		return account
	}
	return ethDIDPrefix + ethcommon.HexToAddress(addr).Hex()
}

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

	// Consensus-gated EIP-55 normalization. Once active, canonicalize any
	// did:pkh:eip155 owner to its checksummed form so the same Ethereum
	// address cannot fragment a balance across case variants. Gated by the
	// EvmAddressChecksumActive predicate (see ConsensusParams) so historical
	// deposits keep their as-credited casing and a reindex stays byte-
	// identical to live state.
	if ls.sconf != nil && ls.sconf.ConsensusParams().EvmAddressChecksumActive(deposit.BlockHeight) {
		decodedParams.To = normalizeEthDID(decodedParams.To)
	}
	// if le.VirtualLedger == nil {
	// 	le.VirtualLedger = make(map[string][]LedgerUpdate)
	// }

	// if le.VirtualLedger[decodedParams.To] != nil {
	// 	le.Ls.log.Debug("ledgerExecutor", le.VirtualLedger[decodedParams.To])
	// }

	if err := ls.LedgerDb.StoreLedger(ledger_db.LedgerRecord{
		Id:          deposit.Id,
		BlockHeight: deposit.BlockHeight,
		Amount:      deposit.Amount,
		Asset:       deposit.Asset,
		From:        deposit.From,
		Owner:       decodedParams.To,
		Type:        "deposit",
		TxId:        deposit.Id,
	}); err != nil {
		log.Error("Deposit: ledger write failed", "id", deposit.Id, "owner", decodedParams.To, "err", err)
	}

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

	log.Verbose("balance distribution", "topCount", len(topBalances), "top5", topBal, "below5", belowBal)
	stakedAmt := belowBal / 3
	log.Verbose("staked amount computed", "stakedAmt", stakedAmt)

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

// New constructs a LedgerSystem. The system config is read at the relevant
// call sites (e.g. EvmAddressChecksumActive) rather than snapshotted, so
// per-network activation heights stay live (including sysconfig overrides on
// devnet/mocknet). Tests may pass nil sconf to keep the legacy verbatim
// behavior; the call sites are nil-safe.
func New(
	balanceDb ledger_db.Balances,
	ledgerDb ledger_db.Ledger,
	claimDb ledger_db.InterestClaims,
	actionDb ledger_db.BridgeActions,
	sconf systemconfig.SystemConfig,
) LedgerSystem {
	return &ledgerSystem{
		BalanceDb: balanceDb,
		LedgerDb:  ledgerDb,
		ClaimDb:   claimDb,
		ActionsDb: actionDb,
		sconf:     sconf,
	}
}
