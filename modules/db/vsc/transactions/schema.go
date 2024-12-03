package transactions

type IngestTransactionUpdate struct {
	Id             string
	RequiredAuths  []string
	Type           string
	Version        string
	Nonce          int64
	Tx             map[string]interface{}
	AnchoredBlock  string
	AnchoredId     string
	AnchoredIndex  int64
	AnchoredOpIdx  int64
	AnchoredHeight int64
}

type SetOutputUpdate struct {
	Id       string
	OutputId string
	Index    int64
}
