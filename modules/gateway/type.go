package gateway

type ChainAction struct {
	Ops        []string `json:"ops"`
	ClearedOps string   `json:"cleared_ops"`
}
