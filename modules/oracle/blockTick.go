package oracle

import (
	"log"
	"slices"
	"vsc-node/lib/utils"
	"vsc-node/modules/common"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/oracle/p2p"
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

	// get elected members
	result, err := o.electionDb.GetElectionByHeight(*headHeight)
	if err != nil {
		log.Println("[oracle] failed to get currently elected members.", err)
		return
	}

	members := result.ElectionDataInfo.Members
	memberAccounts := utils.Map(
		members,
		func(e elections.ElectionMember) string { return e.Account },
	)

	var (
		username                = o.conf.Get().HiveUsername
		isAvgPriceBroadcastTick = *headHeight%priceOracleBroadcastInterval == 0
		isChainRelayTick        = *headHeight%btcChainRelayInterval == 0
		isWitness               = slices.Contains(memberAccounts, username)
		isBlockProducer         = witnessSlot != nil &&
			witnessSlot.Account == username &&
			bh%common.CONSENSUS_SPECS.SlotLength == 0
	)

	sig := makeBlockTickSignal(isBlockProducer, isWitness, members)

	if isAvgPriceBroadcastTick {
		err := o.handleBroadcastPriceTickInterval(sig)
		if err != nil {
			log.Println("[oracle] error on broadcastPriceTick interval.", err)
		}
	}

	if isChainRelayTick {
		// o.blockRelaySignal <- blockTickSignal
	}
}

func (o *Oracle) handleBroadcastPriceTickInterval(sig blockTickSignal) error {
	o.broadcastPriceFlags.lock.Lock()
	o.broadcastPriceFlags.isBroadcastTickInterval = true
	o.broadcastPriceFlags.lock.Unlock()

	defer func() {
		o.broadcastPriceFlags.lock.Lock()
		o.broadcastPriceFlags.isBroadcastTickInterval = false
		o.broadcastPriceFlags.lock.Unlock()

		o.priceOracle.AvgPriceMap.Clear()
		o.broadcastPricePoints.Clear()
		o.broadcastPriceSig.Clear()
		o.broadcastPriceBlocks.Clear()
	}()

	// broadcast local average price
	localAvgPrices := o.priceOracle.AvgPriceMap.GetAveragePrices()

	if err := o.BroadcastMessage(p2p.MsgPriceBroadcast, localAvgPrices); err != nil {
		return err
	}

	// make block / sign block
	var err error

	if sig.isBlockProducer {
		priceBlockProducer := &priceBlockProducer{o}
		err = priceBlockProducer.handleSignal(&sig, localAvgPrices)
	} else if sig.isWitness {
		priceBlockWitness := &priceBlockWitness{o}
		err = priceBlockWitness.handleSignal(&sig)
	}

	return err
}
