package transactionpool

type VSCTransactionShell struct {
	Type    string               `json:"__t" jsonschema:"required"`
	Version string               `json:"__v" jsonschema:"required"`
	Headers VSCTransactionHeader `json:"headers" jsonschema:"required"`
	Tx      []VSCTransactionOp   `json:"tx" jsonschema:"required"`
}

type VSCTransactionHeader struct {
	Nonce         uint64   `json:"nonce"`
	RequiredAuths []string `json:"required_auths"`
	RcLimit       uint64   `json:"rc_limit"` // Optional, used for offchain transactions
	NetId         string   `json:"net_id"`
}

type VSCTransactionOp struct {
	Type    string `json:"type"`
	Payload []byte `json:"payload"`

	RequiredAuths struct {
		Active  []string
		Posting []string
	} `json:"-" refmt:"-" bson:"-"`
}

type VSCTransactionSignStruct struct {
	Type    string                 `json:"__t" jsonschema:"required"`
	Version string                 `json:"__v" jsonschema:"required"`
	Headers VSCTransactionHeader   `json:"headers" jsonschema:"required"`
	Tx      []VSCTransactionSignOp `json:"tx" jsonschema:"required"`
}

type VSCTransactionSignOp struct {
	Type    string      `json:"type"`
	Payload interface{} `json:"payload"`
}
