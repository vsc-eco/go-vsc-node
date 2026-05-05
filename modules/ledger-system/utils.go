package ledgerSystem

import (
	"slices"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
)

var transferableAssetTypes = []string{"hive", "hbd", "hbd_savings"}
var assetTypes = slices.Concat(transferableAssetTypes, []string{"hive_consensus"})

const ETH_REGEX = "^0x[a-fA-F0-9]{40}$"
const HIVE_REGEX = `^[a-z][0-9a-z\-]*[0-9a-z](\.[a-z][0-9a-z\-]*[0-9a-z])*$`

const HBD_INSTANT_FEE = int64(1) // 1% or 100 bps
const HBD_INSTANT_MIN = int64(1) // 0.001 HBD
const HBD_FEE_RECEIVER = "vsc.dao"

type FilterLedgerParams struct {
	//Allow all transactions in all blocks up to this height
	FinalHeight uint64
	BelowBIdx   int64
	BelowOpIdx  int64
}

// In memory filter for ledger ops for querying virtualOps cache
// Use this for writing safer queries against in memory stack
func FilterLedgerOps(query FilterLedgerParams, array []LedgerUpdate) []LedgerUpdate {
	outArray := make([]LedgerUpdate, 0)
	for _, v := range array {
		allowed := false

		if v.BlockHeight < query.FinalHeight {
			allowed = true
		} else if v.BlockHeight == query.FinalHeight {
			if v.BIdx < query.BelowBIdx {
				if v.OpIdx < query.BelowOpIdx {
					allowed = true
				}
			}
		}

		if allowed {
			outArray = append(outArray, v)
		}
	}

	return outArray
}

func ExecuteOplog(oplog []OpLogEvent, startHeight uint64, endBlock uint64) struct {
	accounts      []string
	ledgerRecords []LedgerUpdate
	actionRecords []ledgerDb.ActionRecord
} {
	affectedAccounts := map[string]bool{}

	ledgerRecords := make([]LedgerUpdate, 0)
	actionRecords := make([]ledgerDb.ActionRecord, 0)

	for _, v := range oplog {
		if v.Type == "transfer" {
			affectedAccounts[v.From] = true
			affectedAccounts[v.To] = true

			ledgerRecords = append(ledgerRecords, LedgerUpdate{
				Id:          v.Id,
				From:        v.From,
				To:          v.To,
				Amount:      v.Amount,
				Asset:       v.Asset,
				Type:        "transfer",
				BlockHeight: endBlock,
			})
		}
		if v.Type == "withdraw" {
			affectedAccounts[v.From] = true

			ledgerRecords = append(ledgerRecords, LedgerUpdate{
				Id:          v.Id,
				From:        v.From,
				Amount:      v.Amount,
				Asset:       v.Asset,
				Type:        "withdraw",
				BlockHeight: endBlock,
			})

			actionRecords = append(actionRecords, ledgerDb.ActionRecord{
				Id:          v.Id,
				Amount:      v.Amount,
				Asset:       v.Asset,
				To:          v.To,
				Memo:        v.Memo,
				TxId:        v.Id,
				Status:      "pending",
				Type:        "withdraw",
				BlockHeight: v.BlockHeight,
			})
		}
		if v.Type == "stake" {
			affectedAccounts[v.From] = true

			ledgerRecords = append(ledgerRecords, LedgerUpdate{
				Id:          v.Id,
				From:        v.From,
				Amount:      v.Amount,
				Asset:       "hbd",
				Type:        "stake",
				BlockHeight: endBlock,
			})

			actionRecords = append(actionRecords, ledgerDb.ActionRecord{
				Id:          v.Id,
				Amount:      v.Amount,
				Asset:       "hbd_savings",
				To:          v.To,
				Memo:        v.Memo,
				TxId:        v.Id,
				Status:      "pending",
				Type:        "stake",
				BlockHeight: endBlock,
			})

		}
		if v.Type == "unstake" {
			affectedAccounts[v.From] = true

			ledgerRecords = append(ledgerRecords, LedgerUpdate{
				Id:          v.Id,
				BlockHeight: endBlock,
				Amount:      v.Amount,
				Asset:       "hbd_savings",
				From:        v.From,
				Type:        "unstake",
			})
			actionRecords = append(actionRecords, ledgerDb.ActionRecord{
				Id:          v.Id,
				Amount:      v.Amount,
				Asset:       "hbd_savings",
				To:          v.To,
				Memo:        v.Memo,
				TxId:        v.Id,
				Status:      "pending",
				Type:        "unstake",
				BlockHeight: endBlock,
			})
		}
		if v.Type == "consensus_stake" {
			ledgerRecords = append(ledgerRecords, LedgerUpdate{
				Id:          v.Id + ":hive",
				BlockHeight: endBlock,
				Amount:      v.Amount,
				Asset:       "hive",
				From:        v.From,
				Type:        "consensus_stake",
			})
			ledgerRecords = append(ledgerRecords, LedgerUpdate{
				Id:          v.Id + ":hive_consensus",
				BlockHeight: endBlock,
				Amount:      v.Amount,
				Asset:       "hive_consensus",
				To:          v.To,
				Type:        "consensus_stake",
			})
		}
		if v.Type == "consensus_unstake" {
			ledgerRecords = append(ledgerRecords, LedgerUpdate{
				Id:          v.Id,
				BlockHeight: endBlock,
				Amount:      v.Amount,
				Asset:       "hive_consensus",
				From:        v.From,
				Type:        "consensus_unstake",
			})

			actionRecords = append(actionRecords, ledgerDb.ActionRecord{
				Id:     v.Id,
				Amount: v.Amount,
				Asset:  "-",
				To:     v.To,
				Memo:   v.Memo,
				TxId:   v.Id,
				Status: "pending",
				Type:   "consensus_unstake",
				Params: map[string]interface{}{
					"epoch": v.Params["epoch"],
				},
				BlockHeight: endBlock,
			})
		}
	}

	accounts := make([]string, 0)
	for k := range affectedAccounts {
		accounts = append(accounts, k)
	}

	return struct {
		accounts      []string
		ledgerRecords []LedgerUpdate
		actionRecords []ledgerDb.ActionRecord
	}{
		accounts,
		ledgerRecords,
		actionRecords,
	}
}

func NewSession(ledgerState *LedgerState) LedgerSession {
	return &ledgerSession{
		state: ledgerState,

		oplog:     make([]OpLogEvent, 0),
		ledgerOps: make([]LedgerUpdate, 0),
		balances:  make(map[string]*int64),
		idCache:   make(map[string]int),
	}
}
