package ledgerDb

import (
	"vsc-node/modules/aggregate"
)

type Ledger interface {
	aggregate.Plugin
	StoreLedger(...LedgerRecord)
	GetLedgerAfterHeight(account string, blockHeight uint64, asset string, limit *int64) (*[]LedgerRecord, error)
	GetLedgerRange(account string, start uint64, end uint64, asset string, options ...LedgerOptions) (*[]LedgerRecord, error)
	//Gets distinct accounts on or after a block height
	//Used to indicate whether balance has been updated or not
	GetDistinctAccountsRange(startBlock, endBlock uint64) ([]string, error)
}

type Balances interface {
	aggregate.Plugin
	GetBalanceRecord(account string, blockHeight uint64) (*BalanceRecord, error)
	UpdateBalanceRecord(record BalanceRecord) error
	GetAll(blockHeight uint64) []BalanceRecord
}

type InterestClaims interface {
	aggregate.Plugin
	GetLastClaim(blockHeight uint64) *ClaimRecord
	SaveClaim(claimRecord ClaimRecord)
}

type GatewayLedger interface {
	aggregate.Plugin
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
}

type BalanceRecord struct {
	Account           string `bson:"account"`
	BlockHeight       uint64 `bson:"block_height"`
	Hive              int64  `bson:"hive"`
	HIVE_CONSENSUS    int64  `bson:"hive_consensus"`
	HBD               int64  `bson:"hbd"`
	HBD_SAVINGS       int64  `bson:"hbd_savings"`
	HBD_AVG           int64  `bson:"hbd_avg"`
	HBD_CLAIM_HEIGHT  uint64 `bson:"hbd_claim,omitempty"`
	HBD_MODIFY_HEIGHT uint64 `bson:"hbd_modify,omitempty"`
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

type LedgerRecord struct {
	//Unique ID of the operation
	Id          string `bson:"id"`
	Amount      int64  `bson:"amount"`
	BlockHeight uint64 `bson:"block_height"`
	From        string `bson:"from"`
	Owner       string `bson:"owner"`
	Type        string `bson:"t"`
	Asset       string `bson:"tk"`
	TxId        string `bson:"tx_id"`

	BIdx int64
	//Op Index: Index of the operation in the TX
	OpIdx int64
}

type BridgeActions interface {
	aggregate.Plugin
	StoreAction(withdraw ActionRecord)
	ExecuteComplete(ids ...string)
	Get(id string) (*ActionRecord, error)
	SetStatus(id string, status string)
	GetPendingActions(bh uint64, t ...string) ([]ActionRecord, error)
	GetPendingActionsByEpoch(epoch uint64, t ...string) ([]ActionRecord, error)
}

type ILedgerExecutor interface {
}

type SideEffect interface {
	Type() string
	Execute(le *ILedgerExecutor)
}

// TODO: Flesh out to build more modular op creation
type IAction interface {
	Craft()
}

type ActionRecord struct {
	Id     string `bson:"id"`
	Status string `bson:"status"`
	Amount int64  `bson:"amount"`
	Asset  string `bson:"asset"`
	To     string `bson:"to"`
	Memo   string `bson:"memo"`
	TxId   string `bson:"withdraw_id"`
	Type   string `bson:"type"`

	//Extra stored data
	Params      map[string]interface{} `bson:"data"`
	BlockHeight uint64                 `bson:"block_height"`
}

type WithdrawalSideEffect struct {
}

type LedgerOptions struct {
	//Ledger option type
	OpType []string
}
