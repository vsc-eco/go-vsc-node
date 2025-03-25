package stateEngine

type TxPacket struct {
	TxId string
	Ops  []VSCTransaction
}

type TxOutput struct {
	Ok   bool
	Logs []string
}

type TxResult struct {
	Success bool
	Ret     string
}

// More information about the TX
type TxSelf struct {
	TxId          string
	BlockId       string
	BlockHeight   uint64
	Index         int
	OpIndex       int
	Timestamp     string
	RequiredAuths []string
}
