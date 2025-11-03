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
		OpLog: state.Oplog,
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
	if asset == "hbd" {
		ledgerResults, _ := ls.LedgerDb.GetLedgerRange(account, recordHeight, blockHeight, asset, ledger_db.LedgerOptions{
			OpType: []string{"unstake", "deposit"},
		})

		balAdjust := int64(0)

		for _, v := range *ledgerResults {
			balAdjust += v.Amount
		}

		return balRecord.HBD + balAdjust
	} else if asset == "hive" {
		ledgerResults, _ := ls.LedgerDb.GetLedgerRange(account, recordHeight, blockHeight, asset, ledger_db.LedgerOptions{
			OpType: []string{"deposit"},
		})

		balAdjust := int64(0)

		for _, v := range *ledgerResults {
			balAdjust += v.Amount
		}

		return balRecord.Hive + balAdjust
	} else if asset == "hbd_savings" {

		ledgerResults, _ := ls.LedgerDb.GetLedgerRange(account, recordHeight, blockHeight, asset, ledger_db.LedgerOptions{
			OpType: []string{"stake"},
		})

		stakeBal := int64(0)

		for _, v := range *ledgerResults {
			stakeBal += v.Amount
		}

		return balRecord.HBD_SAVINGS + stakeBal
	} else if asset == "hive_consensus" {
		return balRecord.HIVE_CONSENSUS
	} else {
		return 0
	}

	// LedgerResults, _ := ls.LedgerDb.GetLedgerRange(account, lastHeight, blockHeight, asset)

	// fmt.Println("LedgerResults.len()", len(*LedgerResults))
	// for _, v := range *LedgerResults {
	// 	balRecord = balRecord + v.Amount
	// }
	// return balRecord
}
