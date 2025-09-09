package oracle

import (
	"vsc-node/modules/common"
	stateEngine "vsc-node/modules/state-processing"
)

func (o *Oracle) blockTick(bh uint64, headHeight *uint64) {
	if headHeight == nil {
		return
	}

	slotInfo := stateEngine.CalculateSlotInfo(bh)
	schedule := o.stateEngine.GetSchedule(slotInfo.StartHeight)

	var witnessSlot *stateEngine.WitnessSlot
	for _, slot := range schedule {
		if slot.SlotHeight == slotInfo.StartHeight {
			witnessSlot = &slot
			break
		}
	}

	var (
		isAveragePriceBroadcastTick = *headHeight%priceOracleBroadcastInterval == 0
		isChainRelayTick            = *headHeight%btcChainRelayInterval == 0
		isBlockProducer             = witnessSlot != nil &&
			witnessSlot.Account == o.conf.Get().HiveUsername &&
			bh%common.CONSENSUS_SPECS.SlotLength == 0
		isWitness bool
	)

	blockTickSignal := makeBlockTickSignal(isBlockProducer, isWitness)

	if isAveragePriceBroadcastTick {
		o.broadcastPriceSignal <- blockTickSignal
	}

	if isBlockProducer && isChainRelayTick {
		o.blockRelaySignal <- blockTickSignal
	}
}
