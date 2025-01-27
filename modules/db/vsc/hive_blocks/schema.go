package hive_blocks

import "github.com/vsc-eco/hivego"

type DocumentType string

const (
	DocumentTypeHiveBlock DocumentType = "block"
	DocumentTypeMetadata  DocumentType = "metadata"
)

type Document struct {
	Type               DocumentType `json:"type" bson:"type"`
	Block              *HiveBlock   `json:"block,omitempty" bson:"block,omitempty"`
	LastProcessedBlock *int         `json:"last_processed_block,omitempty" bson:"last_processed_block,omitempty"`
	LastStoredBlock    *int         `json:"last_stored_block,omitempty" bson:"last_stored_block,omitempty"`
}

// the simplified version of a hive block we store
type HiveBlock struct {
	BlockNumber  int                `json:"block_number" bson:"block_number"`
	BlockID      string             `json:"block_id" bson:"block_id"`
	Timestamp    string             `json:"timestamp" bson:"timestamp"`
	MerkleRoot   string             `json:"merkle_root" bson:"merkle_root"`
	Transactions []Tx               `json:"transactions" bson:"transactions"`
	VirtualOps   []hivego.VirtualOp `json:"virtual_ops" bson:"virtual_ops"`
}

// a tx that can be stored inside a hive block
type Tx struct {
	Index         int                `json:"index" bson:"index"`
	TransactionID string             `json:"transaction_id" bson:"transaction_id"`
	Operations    []hivego.Operation `json:"operations" bson:"operations"`
}

type VirtualOp struct {
	Block int64 `json:"block" bson:"block"`
	Op    struct {
		Type  string                 `json:"type" bson:"type"`
		Value map[string]interface{} `json:"value" bson:"value"`
	} `json:"op" bson:"op"`
	TxId  string `json:"tx_id" bson:"tx_id"`
	BIdx  int64  `json:"b_idx" bson:"b_idx"`
	OpIdx int64  `json:"op_idx" bson:"op_idx"`
}
