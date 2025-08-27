package sdk

type Env struct {
	ContractId string `json:"contract.id"`

	//About the calling tx & operation
	TxId    string `json:"tx.id"`
	Index   uint64 `json:"tx.index"`
	OpIndex uint64 `json:"tx.op_index"`

	//Block section
	BlockId     string `json:"block.id"`
	BlockHeight uint64 `json:"block.height"`
	Timestamp   string `json:"block.timestamp"`

	//Original creator of the transaction triggering this operation; Must be a user address
	Sender Sender `json:"sender"`
	//The address that is calling the contract; It can be contract address or user address
	Caller Caller `json:"caller"`

	//Who pays for the RC fee. Can be used in other contexts.
	//Proper RC payer support is not implemented yet.
	Payer Address `json:"payer"`

	Intents []Intent `json:"intents"`
}
