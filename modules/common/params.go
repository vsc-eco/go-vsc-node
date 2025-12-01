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

const HBD_UNSTAKE_BLOCKS = uint64(86400)

type BLOCKTXTYPE int

const (
	BlockTypeNull BLOCKTXTYPE = iota
	BlockTypeTransaction
	BlockTypeOutput
	BlockTypeAnchor
	BlockTypeOplog
	BlockTypeRcUpdate
)
