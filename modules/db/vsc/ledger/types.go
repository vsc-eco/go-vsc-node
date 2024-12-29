package ledgerDb

import (
	"vsc-node/modules/aggregate"

	"go.mongodb.org/mongo-driver/bson"
)

type Ledger interface {
	aggregate.Plugin
	StoreLedger(LedgerRecord)
	GetLedgerAfterHeight(account string, blockHeight int64, asset string, limit *int64) (*[]LedgerRecord, error)
}

type Balances interface {
	aggregate.Plugin
	GetBalanceRecord(account string, blockHeight int64, asset string) (int64, error)
}

type GatewayLedger interface {
	aggregate.Plugin
}

type BalanceRecord struct {
	Account     string `bson:"account"`
	BlockHeight int64  `bson:"block_height"`
	Hive        int64  `bson:"t_hive"`
	HBD         int64  `bson:"t_hbd"`
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
}

type BridgeActions interface {
	aggregate.Plugin
	StoreWithdrawal(withdraw ActionRecord)
	MarkComplete(id string)
}

type ILedgerExecutor interface {
}

type SideEffect interface {
	Type() string
	Execute(le *ILedgerExecutor) bson.M
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
	Data map[string]interface{} `bson:"data"`

	SideEffect *SideEffect
}

type WithdrawalSideEffect struct {
}
