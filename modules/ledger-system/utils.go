package ledgerSystem

import (
	"slices"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
)

var transferableAssetTypes = []string{"hive", "hbd", "hbd_savings"}

// AssetDelegation is a virtual (non-transferable) per-edge balance: the net HIVE a
// delegator has consensus-staked to a specific node, keyed by a composite owner
// (see DelegationEdgeKey). It reuses the normal balance machinery (GetBalance /
// snapshot / in-session cache); it is NOT a real spendable asset. Consensus 0.2.0+.
const AssetDelegation = "delegation"

// delegationEdgeSep separates from/to in a delegation edge owner key. A single ":"
// appears inside normalized accounts ("hive:alice"), so "::" is used as the joiner
// and cannot occur inside either side.
const delegationEdgeSep = "::"

var assetTypes = slices.Concat(transferableAssetTypes, []string{"hive_consensus", AssetDelegation})

// DelegationEdgeKey returns the composite owner key for the (from -> to) delegation
// edge: the net stake `from` has delegated to node `to`. Read it with
// GetBalance(DelegationEdgeKey(from, to), height, AssetDelegation).
func DelegationEdgeKey(from, to string) string {
	return from + delegationEdgeSep + to
}

// opDelegated reports whether an oplog event was stamped with delegated-mode
// semantics. The flag is set by ConsensusStake/ConsensusUnstake from the
// version gate (StateEngine.delegatedStakeActive) at the moment the op is
// applied, so ExecuteOplog — a pure function of the oplog — stays deterministic
// and consensus-gated without needing the election DB itself.
func opDelegated(v OpLogEvent) bool {
	if v.Params == nil {
		return false
	}
	d, _ := v.Params["delegated"].(bool)
	return d
}

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
				Id:          v.Id + "#in",
				Owner:       v.From,
				Amount:      -v.Amount,
				Asset:       v.Asset,
				Type:        "transfer",
				BlockHeight: endBlock,
			})
			ledgerRecords = append(ledgerRecords, LedgerUpdate{
				Id:          v.Id + "#out",
				Owner:       v.To,
				Amount:      v.Amount,
				Asset:       v.Asset,
				Type:        "transfer",
				BlockHeight: endBlock,
			})
		}
		if v.Type == "withdraw" {
			affectedAccounts[v.From] = true

			ledgerRecords = append(ledgerRecords, LedgerUpdate{
				Id:          v.Id + "#in",
				Owner:       v.From,
				Amount:      -v.Amount,
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
				Id:          v.Id + "#in",
				Owner:       v.From,
				Amount:      -v.Amount,
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
				Id:          v.Id + "#in",
				BlockHeight: endBlock,
				Amount:      -v.Amount,
				Asset:       "hbd_savings",
				Owner:       v.From,
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
			// Debit the staker's spendable hive; credit the node's aggregate
			// hive_consensus bond (unchanged in both eras — the node bond still
			// feeds election weight + pendulum). affectedAccounts intentionally
			// untouched, matching the legacy consensus path.
			ledgerRecords = append(ledgerRecords, LedgerUpdate{
				Id:          v.Id + "#in",
				BlockHeight: endBlock,
				Amount:      -v.Amount,
				Asset:       "hive",
				Owner:       v.From,
				Type:        "consensus_stake",
			})
			ledgerRecords = append(ledgerRecords, LedgerUpdate{
				Id:          v.Id + "#out",
				BlockHeight: endBlock,
				Amount:      v.Amount,
				Asset:       "hive_consensus",
				Owner:       v.To,
				Type:        "consensus_stake",
			})
			if opDelegated(v) {
				// Record the per-edge delegation so the delegator (and only the
				// delegator) can reclaim it later. Composite owner from::to.
				ledgerRecords = append(ledgerRecords, LedgerUpdate{
					Id:          v.Id + "#edge",
					BlockHeight: endBlock,
					Amount:      v.Amount,
					Asset:       AssetDelegation,
					Owner:       DelegationEdgeKey(v.From, v.To),
					Type:        "consensus_stake",
				})
			}
		}
		if v.Type == "consensus_unstake" {
			if opDelegated(v) {
				// Delegated era: debit the NODE's (v.To) bond, decrement the
				// delegator's edge, and queue the HIVE return to the delegator
				// (v.From) — the matured-release path credits action.To.
				ledgerRecords = append(ledgerRecords, LedgerUpdate{
					Id:          v.Id + "#in",
					BlockHeight: endBlock,
					Amount:      -v.Amount,
					Asset:       "hive_consensus",
					Owner:       v.To,
					Type:        "consensus_unstake",
				})
				ledgerRecords = append(ledgerRecords, LedgerUpdate{
					Id:          v.Id + "#edge",
					BlockHeight: endBlock,
					Amount:      -v.Amount,
					Asset:       AssetDelegation,
					Owner:       DelegationEdgeKey(v.From, v.To),
					Type:        "consensus_unstake",
				})
				actionRecords = append(actionRecords, ledgerDb.ActionRecord{
					Id:     v.Id,
					Amount: v.Amount,
					Asset:  "-",
					To:     v.From, // release returns HIVE to the delegator
					Memo:   v.Memo,
					TxId:   v.Id,
					Status: "pending",
					Type:   "consensus_unstake",
					Params: map[string]interface{}{
						"epoch": v.Params["epoch"],
						"node":  v.To,
					},
					BlockHeight: endBlock,
				})
			} else {
				// Legacy era (< 0.2.0): unchanged — debit the signer's own bond,
				// release returns to v.To.
				ledgerRecords = append(ledgerRecords, LedgerUpdate{
					Id:          v.Id + "#in",
					BlockHeight: endBlock,
					Amount:      -v.Amount,
					Asset:       "hive_consensus",
					Owner:       v.From,
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
	}
	// assets := []string{"hbd", "hive", "hbd_savings"}

	// fmt.Println("Affected Accounts", affectedAccounts)
	// //Cleanup!
	// for k := range affectedAccounts {
	// 	ledgerBalances := map[string]int64{}
	// 	for _, asset := range assets {
	// 		//As of block X or below
	// 		bal := ls.GetBalance(k, endBlock, asset)
	// 		fmt.Println("bal", bal)
	// 		ledgerBalances[asset] = bal

	// 		// LedgerUpdates, _ := ls.LedgerDb.GetLedgerRange(k, startHeight, endBlock, asset)

	// 		// for _, v := range *LedgerUpdates {
	// 		// 	ledgerBalances[asset] += v.Amount
	// 		// }
	// 	}
	// 	ls.BalanceDb.UpdateBalanceRecord(k, endBlock, ledgerBalances)
	// }

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
