package oracle

import (
	"log"
	"slices"
	"vsc-node/lib/utils"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/oracle/chain"
	"vsc-node/modules/oracle/p2p"
	stateEngine "vsc-node/modules/state-processing"
)

const blockHeightThreshold = 10

var (
	// _ BlockTickHandler = &price.PriceOracle{}
	_ BlockTickHandler = &chain.ChainOracle{}
)

type BlockTickHandler interface {
	HandleBlockTick(p2p.BlockTickSignal, p2p.OracleP2PSpec)
}

func (o *Oracle) blockTick(bh uint64, headHeight *uint64) {
	if headHeight == nil {
		return
	}

	blockDiff := *headHeight - bh
	if blockDiff > blockHeightThreshold {
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
		username = o.conf.Get().HiveUsername
		// isAvgPriceBroadcastTick = *headHeight%priceOracleBroadcastInterval == 0
		isChainRelayTick = *headHeight%chainRelayInterval == 0
		isWitness        = slices.Contains(memberAccounts, username)
		isProducer       = witnessSlot != nil &&
			witnessSlot.Account == username
	)

	signal := p2p.BlockTickSignal{
		IsProducer:     isProducer,
		IsWitness:      isWitness,
		ElectedMembers: members,
	}

	// if isAvgPriceBroadcastTick {
	// 	go o.priceOracle.HandleBlockTick(signal, o)
	// }

	if isChainRelayTick {
		go o.chainOracle.HandleBlockTick(signal, o)
	}
}
