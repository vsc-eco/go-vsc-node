package hive_blocks

import "github.com/vsc-eco/hivego"

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
	Index         int                `json:"index" bson:"index"`
	TransactionID string             `json:"transaction_id" bson:"transaction_id"`
	Operations    []hivego.Operation `json:"operations" bson:"operations"`
}
