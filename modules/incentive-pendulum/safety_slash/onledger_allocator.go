package safetyslash

import (
	"sort"
	"strings"

	"vsc-node/modules/common/params"
	ledger_db "vsc-node/modules/db/vsc/ledger"
	ledgerSystem "vsc-node/modules/ledger-system"
)

// OnLedgerRestitutionAllocator is the consensus-safe replacement for
// MemoryRestitutionQueue. Claims live as ledger rows on
// params.ProtocolSlashRestitutionClaimsAccount with op type
// ledgerSystem.LedgerTypeSafetyRestitutionClaim, written by
// LedgerSystem.EnqueueRestitutionClaim from the vsc.restitution_claim
// block-content tx handler. Allocation reads those rows in deterministic
// FIFO order, debits each via a paired LedgerTypeSafetyRestitutionClaimConsumed
// row, and returns the per-victim payments to SafetySlashConsensusBond
// for the actual victim credit.
//
// Determinism: every node sees the same row set (ledger is replicated),
// orders by (BlockHeight, Id) so the FIFO is stable, and writes consume
// markers with deterministic Ids derived from (slashTxID, evidenceKind,
// claimRowId). Replays converge via Mongo upsert.
type OnLedgerRestitutionAllocator struct {
	LedgerDb ledger_db.Ledger
}

// NewOnLedgerRestitutionAllocator builds an allocator that reads/writes
// the on-ledger queue. Wire it into StateEngine.slashRestitution in place
// of MemoryRestitutionQueue for production paths.
func NewOnLedgerRestitutionAllocator(ldb ledger_db.Ledger) *OnLedgerRestitutionAllocator {
	return &OnLedgerRestitutionAllocator{LedgerDb: ldb}
}

// AllocateHive implements ledgerSystem.SlashRestitutionAllocator. Reads
// every unconsumed claim row at or before blockHeight, FIFO-allocates up
// to slashAmt, writes consume markers, and returns the per-victim
// payments along with the residual that should be burned. Residual ==
// slashAmt when no claims are pending, preserving today's behaviour.
func (a *OnLedgerRestitutionAllocator) AllocateHive(
	slashAmt int64,
	blockHeight uint64,
	txID, evidenceKind, _slashedAccount string,
) ([]ledgerSystem.SlashRestitutionPayment, int64) {
	if a == nil || a.LedgerDb == nil || slashAmt <= 0 {
		return nil, slashAmt
	}
	tx := strings.TrimSpace(txID)
	kind := strings.TrimSpace(evidenceKind)
	if tx == "" || kind == "" {
		// Without slash identity we can't write deterministic consume
		// markers; refuse to allocate rather than corrupt the queue.
		return nil, slashAmt
	}
	claimRecs, err := a.LedgerDb.GetLedgerRange(
		params.ProtocolSlashRestitutionClaimsAccount,
		0,
		blockHeight,
		"hive",
		ledger_db.LedgerOptions{OpType: []string{ledgerSystem.LedgerTypeSafetyRestitutionClaim}},
	)
	if err != nil || claimRecs == nil || len(*claimRecs) == 0 {
		return nil, slashAmt
	}
	consumedRecs, _ := a.LedgerDb.GetLedgerRange(
		params.ProtocolSlashRestitutionClaimsAccount,
		0,
		blockHeight,
		"hive",
		ledger_db.LedgerOptions{OpType: []string{ledgerSystem.LedgerTypeSafetyRestitutionClaimConsumed}},
	)
	// Aggregate consumed totals per claim row Id so we know each claim's
	// remaining balance. The chain-op handler writes one
	// LedgerTypeSafetyRestitutionClaim row per ClaimID; consume markers
	// are per (claim, slashTxID, kind), and Amount is a negative debit.
	consumedByClaim := make(map[string]int64)
	if consumedRecs != nil {
		for _, c := range *consumedRecs {
			// Consume marker Id: <claimRowId>#consumed#<slashTxID>#<kind>
			parts := strings.SplitN(c.Id, "#consumed#", 2)
			if len(parts) != 2 {
				continue
			}
			consumedByClaim[parts[0]] += -c.Amount // Amount is negative; flip to positive consumed
		}
	}

	rows := make([]ledger_db.LedgerRecord, 0, len(*claimRecs))
	rows = append(rows, *claimRecs...)
	// Stable FIFO: order by (BlockHeight asc, Id asc). Two claims at the
	// same height fall back to lexical Id, which is deterministic because
	// Ids are upserted by ClaimID.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].BlockHeight != rows[j].BlockHeight {
			return rows[i].BlockHeight < rows[j].BlockHeight
		}
		return rows[i].Id < rows[j].Id
	})

	var payments []ledgerSystem.SlashRestitutionPayment
	var consumeWrites []ledger_db.LedgerRecord
	remaining := slashAmt
	for _, claim := range rows {
		if remaining <= 0 {
			break
		}
		if claim.Amount <= 0 {
			continue
		}
		alreadyConsumed := consumedByClaim[claim.Id]
		available := claim.Amount - alreadyConsumed
		if available <= 0 {
			continue
		}
		victim := strings.TrimSpace(claim.From)
		if victim == "" {
			continue
		}
		// ClaimID is the part of the row Id after the "safety_restitution_claim#" prefix.
		claimID := strings.TrimPrefix(claim.Id, "safety_restitution_claim#")
		pay := remaining
		if available < pay {
			pay = available
		}
		payments = append(payments, ledgerSystem.SlashRestitutionPayment{
			ClaimID:       claimID,
			VictimAccount: victim,
			Amount:        pay,
		})
		consumeWrites = append(consumeWrites, ledger_db.LedgerRecord{
			Id:          claim.Id + "#consumed#" + tx + "#" + kind,
			TxId:        tx,
			BlockHeight: blockHeight,
			Amount:      -pay,
			Asset:       "hive",
			Owner:       params.ProtocolSlashRestitutionClaimsAccount,
			From:        victim,
			Type:        ledgerSystem.LedgerTypeSafetyRestitutionClaimConsumed,
		})
		remaining -= pay
	}
	if len(consumeWrites) > 0 {
		a.LedgerDb.StoreLedger(consumeWrites...)
	}
	return payments, remaining
}

var _ ledgerSystem.SlashRestitutionAllocator = (*OnLedgerRestitutionAllocator)(nil)
