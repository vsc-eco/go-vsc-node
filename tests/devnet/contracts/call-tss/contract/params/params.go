package params

//tinyjson:json
type Params struct {
	KeyName string `json:"key_name"`
	MsgHex  string `json:"msg_hex,omitempty"`
	Epochs  uint64 `json:"epochs,omitempty"`
}
