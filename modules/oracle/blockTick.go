package oracle

import (
	"context"
	"log"
	"slices"
	"strings"
	"time"
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
	o.logger.Debug("broadcast price block tick.")

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

	// get median prices
	medianPricePoints := o.getMedianPricePoint(localAvgPrices)

	// make block / sign block
	var err error

	if sig.isBlockProducer {
		priceBlockProducer := &priceBlockProducer{o}
		err = priceBlockProducer.handleSignal(&sig, medianPricePoints)
	} else if sig.isWitness {
		priceBlockWitness := &priceBlockWitness{o}
		err = priceBlockWitness.handleSignal(&sig, medianPricePoints)
	}

	return err
}

func (o *Oracle) getMedianPricePoint(
	localAvgPrices map[string]p2p.AveragePricePoint,
) map[string]pricePoint {
	o.broadcastPriceFlags.lock.Lock()
	o.broadcastPriceFlags.isCollectingAveragePrice = true
	o.broadcastPriceFlags.lock.Unlock()

	// room for network latency
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer func() {
		cancel()
		o.broadcastPriceFlags.lock.Lock()
		o.broadcastPriceFlags.isCollectingAveragePrice = false
		o.broadcastPriceFlags.lock.Unlock()
	}()

	<-ctx.Done()

	o.logger.Debug("collecting average prices")

	appBuf := make(map[string]aggregatedPricePoints)

	// updating with local price points
	for k, v := range localAvgPrices {
		sym := strings.ToUpper(k)
		appBuf[sym] = aggregatedPricePoints{
			prices:  []float64{v.Price},
			volumes: []float64{v.Volume},
		}
	}

	// updating with broadcasted price points
	timeThreshold := time.Now().UTC().Add(-time.Hour)
	broadcastedPricePoints := o.broadcastPricePoints.GetMap()

	for sym, pricePoints := range broadcastedPricePoints {
		sym = strings.ToUpper(sym)

		for _, pricePoint := range pricePoints {
			v, ok := appBuf[sym]
			if !ok {
				log.Println("unsupported symbol", sym)
			}

			pricePointExpired := timeThreshold.After(pricePoint.collectedAt)
			if pricePointExpired {
				continue
			}

			v.volumes = append(appBuf[sym].volumes, pricePoint.volume)
			v.prices = append(appBuf[sym].prices, pricePoint.price)

			appBuf[sym] = v
		}
	}

	// calculating the median volumes + prices
	medianPricePoint := make(map[string]pricePoint)
	for sym, app := range appBuf {
		medianPricePoint[sym] = pricePoint{
			price:  getMedian(app.prices),
			volume: getMedian(app.volumes),
			// peerID:      "",
			collectedAt: time.Now().UTC(),
		}
	}

	return medianPricePoint
}
