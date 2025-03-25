package vscBlocks

import "vsc-node/modules/common"

func CalculateRoundInfo(blockHeight uint64) struct {
	StartHeight uint64
	EndHeight   uint64
} {
	mod3 := blockHeight % common.CONSENSUS_SPECS.ScheduleLength

	pastHeight := blockHeight - mod3
	return struct {
		StartHeight uint64
		EndHeight   uint64
	}{
		StartHeight: pastHeight,
		EndHeight:   pastHeight + common.CONSENSUS_SPECS.ScheduleLength,
	}
}
