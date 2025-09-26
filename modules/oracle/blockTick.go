package oracle

import (
	"log"
	"slices"
	"sync"
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
		isChainRelayTick        = *headHeight%chainRelayInterval == 0
		isWitness               = slices.Contains(memberAccounts, username)
		isBlockProducer         = witnessSlot != nil &&
			witnessSlot.Account == username &&
			bh%common.CONSENSUS_SPECS.SlotLength == 0
	)

	sig := p2p.BlockTickSignal{
		IsBlockProducer: isBlockProducer,
		IsWitness:       isWitness,
		ElectedMembers:  members,
	}
	wg := &sync.WaitGroup{}

	if isAvgPriceBroadcastTick {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o.priceOracle.HandleBlockTick(sig, o.broadcastMessage)
		}()
	}

	if isChainRelayTick {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o.handleChainRelayTickInterval(sig)
		}()
	}

	wg.Wait()
}
