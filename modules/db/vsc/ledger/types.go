package ledger_db

import (
	"context"
	"vsc-node/modules/aggregate"
)

type Ledger interface {
	aggregate.Plugin
	StoreLedger(ctx context.Context, records ...LedgerRecord)
	GetLedgerAfterHeight(ctx context.Context, account string, blockHeight uint64, asset string, limit *int64) (*[]LedgerRecord, error)
	GetLedgerRange(ctx context.Context, account string, start uint64, end uint64, asset string, options ...LedgerOptions) (*[]LedgerRecord, error)
	GetLedgersTsRange(ctx context.Context, account *string, txId *string, txTypes []string, asset *Asset, fromBlock *uint64, toBlock *uint64, offset int, limit int) ([]LedgerRecord, error)
	GetRawLedgerRange(ctx context.Context, account *string, txId *string, txTypes []string, asset *Asset, fromBlock *uint64, toBlock *uint64, offset int, limit int) ([]LedgerRecord, error)
	//Gets distinct accounts on or after a block height
	//Used to indicate whether balance has been updated or not
	GetDistinctAccountsRange(ctx context.Context, startBlock, endBlock uint64) ([]string, error)
}

type Balances interface {
	aggregate.Plugin
	GetBalanceRecord(ctx context.Context, account string, blockHeight uint64) (*BalanceRecord, error)
	UpdateBalanceRecord(ctx context.Context, record BalanceRecord) error
	GetAll(ctx context.Context, blockHeight uint64) []BalanceRecord
}

type InterestClaims interface {
	aggregate.Plugin
	GetLastClaim(ctx context.Context, blockHeight uint64) *ClaimRecord
	SaveClaim(ctx context.Context, claimRecord ClaimRecord)
	FindClaims(ctx context.Context, fromBlock *uint64, toBlock *uint64, offset int, limit int) ([]ClaimRecord, error)
}

type ClaimRecord struct {
	BlockHeight uint64 `bson:"block_height"`
	Amount      int64  `bson:"amount"`
	//Claim transaction ID. Can be directly triggered or implicit
	TxId string `bson:"tx_id"`
	//Numbers of accounts received interest
	ReceivedN int `bson:"received_n"`
	//Percent that was observed based on network averages
	ObservedApr float64  `bson:"observed_apr"`
	Timestamp   *string  `json:"timestamp,omitempty" bson:"timestamp,omitempty"`
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
// 	"from": "",
// 	"to": "did:pkh:eip155:1:0x0F8239B80720BA9367B19047c92924e7287b7A35",
// 	"t": "deposit",
// 	"tk": "HBD"
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
	To          string  `json:"to" bson:"to"`
	Type        string  `json:"t" bson:"t"`
	Asset       string  `json:"tk" bson:"tk"`

	Memo   string                 `json:"memo,omitempty" bson:"memo,omitempty"`
	Params map[string]interface{} `json:"params,omitempty" bson:"params,omitempty"`

	BIdx int64
	//Op Index: Index of the operation in the TX
	OpIdx int64
}

// DeltaFor returns the signed balance change this record produces for account.
// + when account is the recipient (To), - when sender (From), 0 otherwise.
func (r LedgerRecord) DeltaFor(account string) int64 {
	var delta int64
	if r.To == account {
		delta += r.Amount
	}
	if r.From == account {
		delta -= r.Amount
	}
	return delta
}

type BridgeActions interface {
	aggregate.Plugin
	StoreAction(ctx context.Context, withdraw ActionRecord)
	ExecuteComplete(ctx context.Context, actionId *string, ids ...string)
	Get(ctx context.Context, id string) (*ActionRecord, error)
	SetStatus(ctx context.Context, id string, status string)
	GetPendingActions(ctx context.Context, bh uint64, t ...string) ([]ActionRecord, error)
	GetPendingActionsByEpoch(ctx context.Context, epoch uint64, t ...string) ([]ActionRecord, error)
	GetActionsRange(ctx context.Context, txId *string, actionId *string, account *string, byTypes []string, asset *Asset, status *string, fromBlock *uint64, toBlock *uint64, offset int, limit int) ([]ActionRecord, error)
	GetAccountPendingConsensusUnstake(ctx context.Context, account string) (int64, error)
	GetActionsByTxId(ctx context.Context, txId string) ([]ActionRecord, error)
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
