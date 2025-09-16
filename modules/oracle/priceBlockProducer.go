package oracle

import (
	"context"
	"log"
	"math"
	"strings"
	"time"
	"vsc-node/modules/oracle/p2p"
)

type priceBlockProducer struct{ *Oracle }

func (p *priceBlockProducer) handleSignal(
	sig *blockTickSignal,
	localAvgPrices map[string]p2p.AveragePricePoint,
) error {
	medianPricePoints := p.getMedianPricePoint(localAvgPrices)

	block, err := p2p.MakeVscBlock(
		p.conf.Get().HiveUsername,
		p.conf.Get().HiveActiveKey,
		medianPricePoints,
	)
	if err != nil {
		return err
	}

	msg := p2p.OracleMessage{Type: p2p.MsgPriceBlock, Data: *block}
	if err := p.BroadcastMessage(&msg); err != nil {
		return err
	}

	return p.pollMedianPriceSignature(sig, block)
}

func (p *priceBlockProducer) getMedianPricePoint(
	localAvgPrices map[string]p2p.AveragePricePoint,
) map[string]pricePoint {
	p.broadcastPriceFlags.lock.Lock()
	p.broadcastPriceFlags.isCollectingAveragePrice = true
	p.broadcastPriceFlags.lock.Unlock()

	// room for network latency
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer func() {
		cancel()
		p.broadcastPriceFlags.lock.Lock()
		p.broadcastPriceFlags.isCollectingAveragePrice = false
		p.broadcastPriceFlags.lock.Unlock()
	}()

	<-ctx.Done()

	type aggregatedPricePoints struct {
		prices  []float64
		volumes []float64
	}

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
	broadcastedPricePoints := p.broadcastPricePoints.GetMap()

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

func (p *priceBlockProducer) pollMedianPriceSignature(
	sig *blockTickSignal,
	block *p2p.VSCBlock,
) error {
	p.broadcastPriceFlags.lock.Lock()
	p.broadcastPriceFlags.isCollectingSignatures = true
	p.broadcastPriceFlags.lock.Unlock()

	// room for network latency
	ctx, cancel := context.WithTimeout(context.Background(), listenDuration)
	defer func() {
		p.broadcastPriceFlags.lock.Lock()
		p.broadcastPriceFlags.isCollectingSignatures = false
		p.broadcastPriceFlags.lock.Unlock()
		cancel()
	}()

	<-ctx.Done()

	sigThreshold := int(math.Ceil(float64(len(sig.electedMembers) * 2 / 3)))
	block.Signatures = make([]string, 0, sigThreshold)

	signedBlocks := p.broadcastPriceSig.Slice()
	for i := range signedBlocks {
		signedBlock := &signedBlocks[i]

		if validateSignedBlock(signedBlock) {
			block.Signatures = append(
				block.Signatures,
				signedBlock.Signatures[0],
			)
		}
	}

	p.submitToContract(block)

	return nil
}

func validateSignedBlock(block *p2p.VSCBlock) bool {
	if len(block.Signatures) != 1 {
		return false
	}

	if err := v.Struct(block); err != nil {
		return false
	}

	// TODO: validate signature
	return true
}
