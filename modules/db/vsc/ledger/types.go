package ledger_db

import (
	"vsc-node/modules/aggregate"
)

type Ledger interface {
	aggregate.Plugin
	StoreLedger(...LedgerRecord) error
	GetLedgerAfterHeight(account string, blockHeight uint64, asset string, limit *int64) (*[]LedgerRecord, error)
	GetLedgerRange(account string, start uint64, end uint64, asset string, options ...LedgerOptions) (*[]LedgerRecord, error)
	GetLedgersTsRange(account *string, txId *string, txTypes []string, asset *Asset, fromBlock *uint64, toBlock *uint64, offset int, limit int) ([]LedgerRecord, error)
	GetRawLedgerRange(account *string, txId *string, txTypes []string, asset *Asset, fromBlock *uint64, toBlock *uint64, offset int, limit int) ([]LedgerRecord, error)
	//Gets distinct accounts on or after a block height
	//Used to indicate whether balance has been updated or not
	GetDistinctAccountsRange(startBlock, endBlock uint64) ([]string, error)
	// DeleteLegacyInterestRecords removes pre-account-keyed interest rows (id
	// has no '#' separator) at the given ledger block_height, so a reprocess of
	// a block first written under the old index-based id scheme overwrites
	// rather than double-credits. Account-keyed rows (id contains '#') are
	// preserved. See ledger-system.ClaimHBDInterest.
	DeleteLegacyInterestRecords(recordBlockHeight uint64, owners []string) error
}

type Balances interface {
	aggregate.Plugin
	GetBalanceRecord(account string, blockHeight uint64) (*BalanceRecord, error)
	UpdateBalanceRecord(record BalanceRecord) error
	// DeleteBalanceRecordsFrom drops an account's snapshot rows at or above
	// fromHeight. Used only by the late-application path of the one-time
	// ledger remediation: GetBalance is snapshot-anchored, so a credit written
	// retroactively below an existing snapshot is invisible forever. Removing
	// those snapshots re-anchors the fold below the credit, and because
	// UpdateBalances rebuilds from prevSnapshot.BlockHeight+1 the credit then
	// lands in the very next rebuild window.
	DeleteBalanceRecordsFrom(account string, fromHeight uint64) error
	GetAll(blockHeight uint64) ([]BalanceRecord, error)
}

type InterestClaims interface {
	aggregate.Plugin
	GetLastClaim(blockHeight uint64) *ClaimRecord
	SaveClaim(claimRecord ClaimRecord)
	FindClaims(fromBlock *uint64, toBlock *uint64, offset int, limit int) ([]ClaimRecord, error)
}

type ClaimRecord struct {
	BlockHeight uint64 `bson:"block_height"`
	Amount      int64  `bson:"amount"`
	//Claim transaction ID. Can be directly triggered or implicit
	TxId string `bson:"tx_id"`
	//Numbers of accounts received interest
	ReceivedN int `bson:"received_n"`
	//Percent that was observed based on network averages
	ObservedApr float64 `bson:"observed_apr"`
	Timestamp   *string `json:"timestamp,omitempty" bson:"timestamp,omitempty"`
}

type BalanceRecord struct {
	Account           string `json:"account" bson:"account"`
	BlockHeight       uint64 `json:"block_height" bson:"block_height"`
	Hive              int64  `json:"hive" bson:"hive"`
	HIVE_CONSENSUS    int64  `json:"hive_consensus" bson:"hive_consensus"`
	HBD               int64  `json:"hbd" bson:"hbd"`
	HBD_SAVINGS       int64  `json:"hbd_savings" bson:"hbd_savings"`
	HBD_AVG           int64  `json:"hbd_avg" bson:"hbd_avg"`
	HBD_CLAIM_HEIGHT  uint64 `json:"hbd_claim" bson:"hbd_claim"`
	HBD_MODIFY_HEIGHT uint64 `json:"hbd_modify" bson:"hbd_modify"`
}

// {
// 	"id": "e719b65bd0a44e7b074029d2dc9d8aa20a7f9823-0",
// 	"amount": 100,
// 	"block_height": 88098888,
// 	"from": "vaultectest7778",
// 	"owner": "did:pkh:eip155:1:0x0F8239B80720BA9367B19047c92924e7287b7A35",
// 	"t": "deposit",
// 	"tk": "HBD",
// 	"tx_id": "e719b65bd0a44e7b074029d2dc9d8aa20a7f9823"
//   }

type Asset string

const (
	AssetHive          Asset = "hive"
	AssetHiveConsensus Asset = "hive_consensus"
	AssetHbd           Asset = "hbd"
	AssetHbdSavings    Asset = "hbd_savings"
)

type LedgerRecord struct {
	//Unique ID of the operation
	Id          string  `json:"id" bson:"id"`
	Amount      int64   `json:"amount" bson:"amount"`
	BlockHeight uint64  `json:"block_height" bson:"block_height"`
	Timestamp   *string `json:"timestamp,omitempty" bson:"timestamp,omitempty"`
	From        string  `json:"from" bson:"from"`
	Owner       string  `json:"owner" bson:"owner"`
	Type        string  `json:"t" bson:"t"`
	Asset       string  `json:"tk" bson:"tk"`
	TxId        string  `json:"tx_id" bson:"tx_id"`

	BIdx int64
	//Op Index: Index of the operation in the TX
	OpIdx int64
}

type BridgeActions interface {
	aggregate.Plugin
	StoreAction(withdraw ActionRecord)
	ExecuteComplete(actionId *string, ids ...string)
	// RevertProcessingToPending re-queues actions stranded in the legacy
	// "processing" state (removed with the cosigner split-brain fix). One-time,
	// idempotent rollout heal; a no-op once no action is "processing". Returns
	// the reverted records for audit logging.
	RevertProcessingToPending() ([]ActionRecord, error)
	Get(id string) (*ActionRecord, error)
	SetStatus(id string, status string)
	GetPendingActions(bh uint64, t ...string) ([]ActionRecord, error)
	GetPendingActionsByEpoch(epoch uint64, t ...string) ([]ActionRecord, error)
	GetActionsRange(txId *string, actionId *string, account *string, byTypes []string, asset *Asset, status *string, fromBlock *uint64, toBlock *uint64, offset int, limit int) ([]ActionRecord, error)
	GetAccountPendingConsensusUnstake(account string) (int64, error)
	GetActionsByTxId(txId string) ([]ActionRecord, error)
}

// TODO: Flesh out to build more modular op creation
// type IAction interface {
// 	Craft()
// }

type ActionRecord struct {
	Id     string `bson:"id"`
	Status string `bson:"status"`
	Amount int64  `bson:"amount"`
	Asset  string `bson:"asset"`
	To     string `bson:"to"`
	Memo   string `bson:"memo"`
	TxId   string `bson:"action_id"`
	Type   string `bson:"type"`

	//Extra stored data
	Params      map[string]interface{} `bson:"data"`
	BlockHeight uint64                 `bson:"block_height"`

	//For api query
	Timestamp *string `json:"timestamp,omitempty" bson:"timestamp,omitempty"`
}

type WithdrawalSideEffect struct {
}

type LedgerOptions struct {
	//Ledger option type
	OpType []string
}
