package ledgerSystem

type LedgerUpdate struct {
	Id string
	//Block Index
	BlockHeight uint64
	//Block Index: Index of the TX in the block
	BIdx int64
	//Op Index: Index of the operation in the TX
	OpIdx int64

	Owner  string
	Amount int64
	Asset  string
	Memo   string
	//transfer, withdraw, stake, unstake
	Type string
}

type OpLogEvent struct {
	To     string `json:"to" bson:"to"`
	From   string `json:"fr" bson:"from"`
	Amount int64  `json:"am" bson:"amount"`
	Asset  string `json:"as" bson:"asset"`
	Memo   string `json:"mo" bson:"memo"`
	Type   string `json:"ty" bson:"type"`

	//Not parted of compiled state
	Id          string `json:"id" bson:"id"`
	BIdx        int64  `json:"-" bson:"-"`
	OpIdx       int64  `json:"-" bson:"-"`
	BlockHeight uint64 `json:"-" bson:"-"`

	//Fee for instant stake unstake
	// Fee int64 `json:"fee,omitempty"`
	Params map[string]interface{} `json:"params,omitempty"`
}

type WithdrawParams struct {
	Id     string `json:"id"`
	From   string `json:"from"`
	To     string `json:"to"`
	Asset  string `json:"asset"`
	Amount int64  `json:"amount"`
	Memo   string `json:"memo"`

	BIdx        int64  `json:"bidx"`
	OpIdx       int64  `json:"opidx"`
	BlockHeight uint64 `json:"block_height"`
}

type ConsensusParams struct {
	Id            string
	From          string
	To            string
	Amount        int64
	Type          string
	BlockHeight   uint64
	ElectionEpoch uint64
}

type LedgerResult struct {
	Ok  bool
	Msg string
}

type TransferOptions struct {
	//Excluded HBD amount that cannot be sent
	Exclusion int64
}

type LedgerSession interface {
	Revert()
	GetBalance(account string, blockHeight uint64, asset string) int64
	ExecuteTransfer(OpLogEvent OpLogEvent, options ...TransferOptions) LedgerResult
	Withdraw(withdraw WithdrawParams) LedgerResult
}

type LedgerSystem interface {
	GetBalance(account string, blockHeight uint64, asset string) int64
}
