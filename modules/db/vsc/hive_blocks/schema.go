package hive_blocks

type HiveBlock struct {
	BlockNumber  int    `json:"block_number" bson:"block_number"`
	BlockID      string `json:"block_id" bson:"block_id"`
	Timestamp    string `json:"timestamp" bson:"timestamp"`
	Transactions []Tx   `json:"transactions" bson:"transactions"`
}

type Tx struct {
	TransactionID string `json:"transaction_id" bson:"transaction_id"`
	Operations    []Op   `json:"operations" bson:"operations"`
}

type Op struct {
	Type  string      `json:"type" bson:"type"`
	Value interface{} `json:"value" bson:"value"`
}
