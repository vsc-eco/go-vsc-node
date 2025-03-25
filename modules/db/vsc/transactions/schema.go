package transactions

import "time"

type IngestTransactionUpdate struct {
	Status         string
	Id             string
	RequiredAuths  []string
	Type           string
	Version        string
	Nonce          uint64
	Tx             interface{}
	AnchoredBlock  *string
	AnchoredId     *string
	AnchoredIndex  *int64
	AnchoredOpIdx  *int64
	AnchoredHeight *int64
}

type SetOutputUpdate struct {
	Id       string
	OutputId string
	Index    int64
}

type TransactionRecord struct {
	Id            string   `json:"id" bson:"id"`
	Status        string   `json:"status" bson:"status"`
	RequiredAuths []string `json:"required_auths" bson:"required_auths"`
	Nonce         int64    `json:"nonce" bson:"nonce"`
	//VSC or Hive
	Type          string                 `json:"type" bson:"type"`
	Version       string                 `json:"__v" bson:"__v"`
	Data          map[string]interface{} `json:"data" bson:"data"`
	AnchoredBlock string                 `json:"anchr_block" bson:"anchr_block"`
	AnchoredId    string                 `json:"anchr_id" bson:"anchr_id"`
	AnchoredIndex int64                  `json:"anchr_index" bson:"anchr_index"`
	AnchoredOpIdx int64                  `json:"anchr_opidx" bson:"anchr_opidx"`
	FirstSeen     time.Time              `json:"first_seen" bson:"first_seen"`
	Output        *struct {
		Id    string `json:"id" bson:"id"`
		Index int64  `json:"index" bson:"index"`
	}
}
