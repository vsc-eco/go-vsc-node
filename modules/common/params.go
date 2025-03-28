package common

var CONSENSUS_SPECS = struct {
	SlotLength     uint64
	EpochLength    uint64
	ScheduleLength uint64
}{
	EpochLength:    7_200, // 6 hours - length between elections
	SlotLength:     10,    //10 * 3 seconds = 30 seconds - length between blocks
	ScheduleLength: 1_200, // 60 * 20 = 1 hour - length of a schedule before it's recalcualted
}

var NETWORK_ID = "vsc-mainnet"

var GATEWAY_WALLET = "vsc.gateway"

var FR_VIRTUAL_ACCOUNT = "system:fr_balance"

var DAO_WALLET = "hive:vsc.dao"

var RC_RETURN_PERIOD uint64 = 120 * 60 * 20 // 5 day cool down period for RCs

type BLOCKTXTYPE int

const (
	BlockTypeNull BLOCKTXTYPE = iota
	BlockTypeTransaction
	BlockTypeOutput
	BlockTypeAnchor
	BlockTypeOplog
	BlockTypeRcUpdate
)
