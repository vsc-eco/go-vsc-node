package hive_blocks

// the simplified version of a hive block we store
type HiveBlock struct {
	BlockNumber  int    `json:"block_number" bson:"block_number"`
	BlockID      string `json:"block_id" bson:"block_id"`
	Timestamp    string `json:"timestamp" bson:"timestamp"`
	MerkleRoot   string `json:"merkle_root" bson:"merkle_root"`
	Transactions []Tx   `json:"transactions" bson:"transactions"`
}

// a tx that can be stored inside a hive block
type Tx struct {
	TransactionID string                   `json:"transaction_id" bson:"transaction_id"`
	Operations    []map[string]interface{} `json:"operations" bson:"operations"`
}
