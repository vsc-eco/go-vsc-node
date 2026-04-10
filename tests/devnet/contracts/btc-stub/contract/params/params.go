package params

// AddBlocksParams matches utxoAddBlocksPayload from
// modules/oracle/chain/handle_block_tick.go on the producer side.
//
//tinyjson:json
type AddBlocksParams struct {
	Blocks    string `json:"blocks"`
	LatestFee int64  `json:"latest_fee"`
}

// InitParams is the JSON payload for the init action.
//
//tinyjson:json
type InitParams struct {
	StartHeight uint64 `json:"start_height"`
}
