package ledgerSystem

import (
	"slices"
	ledger_db "vsc-node/modules/db/vsc/ledger"
)

// Used to represent the global ledger state in the execution environment
type LedgerState struct {
	//List of finalized operations such as transfers, withdrawals.
	//Expected to be identical when block is produced and used during block creation
	Oplog []OpLogEvent
	//Virtual ledger is a cache of all balance changes (virtual and non-virtual)
	//Includes deposits, transfers (in-n-out), withdrawals, and stake/unstake operations (future)
	VirtualLedger map[string][]LedgerUpdate

	//Live calculated gateway balances on the fly
	//Use last saved balance as the starting data
	GatewayBalances map[string]uint64

	//Block height of the operation to be processed at
	BlockHeight uint64

	//Potential database state access

	LedgerDb  ledger_db.Ledger
	ActionDb  ledger_db.BridgeActions
	BalanceDb ledger_db.Balances
}

func (state *LedgerState) Validate() {
	// if state.BlockHeight == 0 {
	// 	panic("invalid ledgerState instance: BlockHeight is 0")
	// }
	if state.LedgerDb == nil || state.ActionDb == nil || state.BalanceDb == nil {
		panic("invalid ledgerState instance: LedgerDb is nil")
	}
}

// func (le *LedgerState) AppendLedger(update LedgerUpdate) {
// 	key := update.Owner + "#" + update.Asset
// 	if le.GatewayBalances[key] == 0 {
// 		le.Ls.GetBalance(update.Owner, update.BlockHeight, update.Asset)
// 	}
// }

func (le *LedgerState) Export() struct {
	Oplog []OpLogEvent
} {
	oplogCP := make([]OpLogEvent, len(le.Oplog))
	copy(oplogCP, le.Oplog)

	return struct {
		Oplog []OpLogEvent
	}{
		Oplog: oplogCP,
	}
}

func (state *LedgerState) Flush() {
	state.VirtualLedger = make(map[string][]LedgerUpdate)
	state.Oplog = make([]OpLogEvent, 0)

	//qq: should this be cleared when flushing?
	state.GatewayBalances = make(map[string]uint64)
}

func (state *LedgerState) Compile(bh uint64) *CompiledResult {
	if len(state.Oplog) == 0 {
		return nil
	}
	oplog := make([]OpLogEvent, 0)
	// copy(oplog, le.Oplog)

	for _, v := range state.Oplog {
		//bh should be == slot height
		if v.BlockHeight <= bh {
			oplog = append(oplog, v)
		}
	}

	return &CompiledResult{
		OpLog: oplog,
	}
}

// Original ledger executor
func (state *LedgerState) SnapshotForAccount(account string, blockHeight uint64, asset string) int64 {
	bal := state.GetBalance(account, blockHeight, asset)

	//le.Ls.log.Debug("getBalance le.VirtualLedger["+account+"]", le.VirtualLedger[account], blockHeight)

	for _, v := range state.VirtualLedger[account] {
		//Must be ledger ops with height below or equal to the current block height
		//Current block height ledger ops are recently executed
		if v.Asset == asset {
			bal += v.Amount
		}
	}
	return bal
}

// GetBalance returns the spendable balance of asset for account as of blockHeight:
// the BalanceDb snapshot field for the asset plus the net of every LedgerDb
// record past the snapshot height.
//
// The snapshot itself is always written correctly by state_engine.UpdateBalances
// (it sums every record of the asset minus the protocol meta rows); only the
// incremental delta below the snapshot height is computed here.
func (ls *LedgerState) GetBalance(account string, blockHeight uint64, asset string) int64 {
	if !slices.Contains(assetTypes, asset) {
		return 0
	}

	balRecordPtr, _ := ls.BalanceDb.GetBalanceRecord(account, blockHeight)

	var recordHeight uint64
	var balRecord ledger_db.BalanceRecord
	if balRecordPtr == nil {
		recordHeight = 0
	} else {
		balRecord = *balRecordPtr
		recordHeight = balRecord.BlockHeight + 1
	}

	var base int64
	switch asset {
	case "hbd":
		base = balRecord.HBD
	case "hive":
		base = balRecord.Hive
	case "hbd_savings":
		base = balRecord.HBD_SAVINGS
	case "hive_consensus":
		base = balRecord.HIVE_CONSENSUS
	default:
		return 0
	}

	// CRIT-1 fix: sum EVERY ledger record of the asset past the snapshot height
	// (natural signs), excluding only the protocol meta rows
	// (IsProtocolMetaLedgerType — the shared source of truth, identical to the
	// snapshot fold in state_engine.UpdateBalances). The previous per-asset
	// OpType ALLOW-list silently dropped OUTFLOWS — transfer, withdraw, stake
	// (hbd) and transfer, withdraw, consensus_stake (hive) all carry the asset's
	// own `tk` but were never subtracted — so between a debit landing and the
	// next slot snapshot fold the spend check read a stale, overstated balance
	// and admitted a second debit of the same funds (double-spend). Summing all
	// records minus the meta rows makes GetBalance == snapshot + remaining-delta
	// by construction, so the delta and the fold can never drift again.
	//
	// Not consensus-version-gated: this is not a forkable change. GetBalance
	// only feeds block PRODUCTION and the cosigner's re-derivation; finalized
	// ledger state is the producer's oplog applied verbatim by IngestOplog (no
	// balance check), and a block finalizes only on a 2/3 matching-CID quorum
	// (one leader per slot). A divergent balance can therefore stall a slot but
	// can neither fork the chain nor change the replay of an already-finalized
	// block.
	ledgerResults, _ := ls.LedgerDb.GetLedgerRange(account, recordHeight, blockHeight, asset)
	balAdjust := int64(0)
	for _, v := range *ledgerResults {
		if IsProtocolMetaLedgerType(v.Type) {
			continue
		}
		balAdjust += v.Amount
	}
	return base + balAdjust
}
