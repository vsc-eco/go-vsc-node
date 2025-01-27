package ledgerDb

import (
	"vsc-node/modules/aggregate"
)

type Ledger interface {
	aggregate.Plugin
	StoreLedger(LedgerRecord)
	GetLedgerAfterHeight(account string, blockHeight int64, asset string, limit *int64) (*[]LedgerRecord, error)
	GetLedgerRange(account string, start int64, end int64, asset string) (*[]LedgerRecord, error)
}

type Balances interface {
	aggregate.Plugin
	GetBalanceRecord(account string, blockHeight int64, asset string) (int64, int64, error)
	UpdateBalanceRecord(account string, blockHeight int64, balances map[string]int64) error
	GetAll(blockHeight int64) []BalanceRecord
}

type InterestClaims interface {
	aggregate.Plugin
	GetLastClaim(blockHeight int) *ClaimRecord
	SaveClaim(blockHeight int, amount int)
}

type GatewayLedger interface {
	aggregate.Plugin
}

type ClaimRecord struct {
	BlockHeight int `bson:"block_height"`
	Amount      int `bson:"amount"`
}

type BalanceRecord struct {
	Account           string `bson:"account"`
	BlockHeight       int64  `bson:"block_height"`
	Hive              int64  `bson:"t_hive"`
	HBD               int64  `bson:"t_hbd"`
	HBD_SAVINGS       int64  `bson:"t_hbd_savings"`
	HBD_AVG           int64  `bson:"t_hbd_avg"`
	HBD_CLAIM_HEIGHT  int64  `bson:"t_hbd_claim"`
	HBD_MODIFY_HEIGHT int64  `bson:"t_hbd_modify"`
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
	BlockHeight int64  `bson:"block_height"`
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
	StoreWithdrawal(withdraw ActionRecord)
	ExecuteComplete(id string)
	Get(id string) (*ActionRecord, error)
	SetStatus(id string, status string)
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
	Params map[string]interface{} `bson:"data"`

	SideEffect *SideEffect
}

type WithdrawalSideEffect struct {
}
