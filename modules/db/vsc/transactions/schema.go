package transactions

import (
	"time"
	ledgerSystem "vsc-node/modules/ledger-system"
)

type IngestTransactionUpdate struct {
	Id                   string
	Status               string
	RequiredAuths        []string
	RequiredPostingAuths []string
	Type                 string
	Version              string
	Nonce                uint64
	OpTypes              []string
	Ops                  []TransactionOperation
	RcLimit              uint64
	AnchoredBlock        *string
	AnchoredId           *string
	AnchoredIndex        *int64
	AnchoredHeight       *uint64
	Ledger               []ledgerSystem.OpLogEvent
}

type SetResultUpdate struct {
	// Id       string
	// OutputId string
	// Index    int64
	Id string
	// Note: This is referring to index in outputs array in oplog
	OpIdx  int
	Ledger *[]ledgerSystem.OpLogEvent
	Status *TransactionStatus
	Output *TransactionOutput
}

type TransactionStatus string

const (
	TransactionStatusUnconfirmed TransactionStatus = "UNCONFIRMED"
	TransactionStatusConfirmed   TransactionStatus = "CONFIRMED"
	TransactionStatusFailed      TransactionStatus = "FAILED"
	TransactionStatusIncluded    TransactionStatus = "INCLUDED"
	TransactionStatusProcessed   TransactionStatus = "PROCESSED"
)

type TransactionOperation struct {
	RequiredAuths []string               `json:"required_auths" bson:"required_auths"`
	Type          string                 `json:"type" bson:"type"`
	Idx           int64                  `json:"idx" bson:"idx"`
	Data          map[string]interface{} `json:"data" bson:"data"`
}

type TransactionOutput struct {
	Id    string `json:"id" bson:"id"`
	Index int64  `json:"index" bson:"index"`
}

type TransactionRecord struct {
	Id     string `json:"id" bson:"id"`
	Status string `json:"status" bson:"status"`

	//Auths involved in the transaction
	RequiredAuths        []string `json:"required_auths" bson:"required_auths"`
	RequiredPostingAuths []string `json:"required_posting_auths,omitempty" bson:"required_posting_auths,omitempty"`
	Nonce                int64    `json:"nonce" bson:"nonce"`

	RcLimit uint64 `json:"rc_limit" bson:"rc_limit"`

	//VSC or Hive
	// TxId    string                 `json:"tx_id,omitempty" bson:"tx_id,omitempty"`
	Type    string `json:"type" bson:"type"`
	Version string `json:"__v" bson:"__v"`
	// Data    map[string]interface{} `json:"data" bson:"data"`
	Ops     []TransactionOperation `json:"ops" bson:"ops"`
	OpTypes []string               `json:"op_types" bson:"op_types"`

	AnchoredBlock string `json:"anchr_block" bson:"anchr_block"`
	AnchoredId    string `json:"anchr_id" bson:"anchr_id"`
	AnchoredIndex int64  `json:"anchr_index" bson:"anchr_index"`
	// AnchoredOpIdx  int64                      `json:"anchr_opidx" bson:"anchr_opidx"`
	AnchoredTs     *string `json:"anchr_ts,omitempty" bson:"anchr_ts,omitempty"`
	AnchoredHeight uint64  `json:"anchr_height" bson:"anchr_height"`

	FirstSeen time.Time                  `json:"first_seen" bson:"first_seen"`
	Ledger    *[]ledgerSystem.OpLogEvent `json:"ledger,omitempty" bson:"ledger,omitempty"`
	Output    *TransactionOutput         `json:"output,omitempty" bson:"output,omitempty"`
}
