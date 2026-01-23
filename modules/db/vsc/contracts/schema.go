package contracts

import wasm_runtime "vsc-node/modules/wasm/runtime"

type Contract struct {
	Id             string               `bson:"id,omitempty"`
	Code           string               `bson:"code"`
	Name           string               `bson:"name"`
	Description    string               `bson:"description"`
	Creator        string               `bson:"creator"`
	Owner          string               `bson:"owner"`
	TxId           string               `bson:"tx_id"`
	CreationHeight uint64               `bson:"creation_height"`
	CreationTs     *string              `bson:"creation_ts,omitempty"`
	Runtime        wasm_runtime.Runtime `bson:"runtime"`
	Latest         bool                 `bson:"latest,omitempty"`
}

type Intent struct {
	Type string            `json:"type"`
	Args map[string]string `json:"args"`
}
