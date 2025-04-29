package transactions

import (
	"time"
	ledgerSystem "vsc-node/modules/ledger-system"
)

type IngestTransactionUpdate struct {
	Id             string
	Status         string
	RequiredAuths  []string
	Type           string
	Version        string
	Nonce          uint64
	Tx             interface{}
	RcLimit        uint64
	AnchoredBlock  *string
	AnchoredId     *string
	AnchoredIndex  *int64
	AnchoredOpIdx  *int64
	AnchoredHeight *uint64
	Ledger         []ledgerSystem.OpLogEvent
}

type SetResultUpdate struct {
	// Id       string
	// OutputId string
	// Index    int64
	Id     string
	Ledger *[]ledgerSystem.OpLogEvent
	Output *struct {
		Id    string `json:"id" bson:"id"`
		Index int64  `json:"index" bson:"index"`
	}
}

type TransactionStatus string

const (
	TransactionStatusUnconfirmed TransactionStatus = "UNCONFIRMED"
	TransactionStatusConfirmed   TransactionStatus = "CONFIRMED"
	TransactionStatusFailed      TransactionStatus = "FAILED"
	TransactionStatusIncluded    TransactionStatus = "INCLUDED"
	TransactionStatusProcessed   TransactionStatus = "PROCESSED"
)

type TransactionRecord struct {
	Id            string   `json:"id" bson:"id"`
	Status        string   `json:"status" bson:"status"`
	RequiredAuths []string `json:"required_auths" bson:"required_auths"`
	Nonce         int64    `json:"nonce" bson:"nonce"`
	RcLimit       uint64   `json:"rc_limit" bson:"rc_limit"`
	//VSC or Hive
	Type           string                     `json:"type" bson:"type"`
	Version        string                     `json:"__v" bson:"__v"`
	Data           map[string]interface{}     `json:"data" bson:"data"`
	AnchoredBlock  string                     `json:"anchr_block" bson:"anchr_block"`
	AnchoredId     string                     `json:"anchr_id" bson:"anchr_id"`
	AnchoredIndex  int64                      `json:"anchr_index" bson:"anchr_index"`
	AnchoredOpIdx  int64                      `json:"anchr_opidx" bson:"anchr_opidx"`
	AnchoredTs     *string                    `json:"anchr_ts,omitempty" bson:"anchr_ts,omitempty"`
	AnchoredHeight uint64                     `json:"anchr_height" bson:"anchr_height"`
	FirstSeen      time.Time                  `json:"first_seen" bson:"first_seen"`
	Ledger         *[]ledgerSystem.OpLogEvent `json:"ledger,omitempty" bson:"ledger,omitempty"`
	Output         *struct {
		Id    string `json:"id" bson:"id"`
		Index int64  `json:"index" bson:"index"`
	}
}
