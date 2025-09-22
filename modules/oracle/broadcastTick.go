package oracle

import (
	"encoding/json"
	"errors"
	"log"
	"math"
	"strings"
	"time"
	"vsc-node/modules/oracle/p2p"
)

// assumed to be locked:
//   - `o.broadcastPricePoints *threadsafe.Map[string, []pricePoint]`
//   - `o.broadcastPriceSig    *threadsafe.Slice[p2p.OracleBlock]`
//   - `o.broadcastPriceBlocks *threadsafe.Slice[p2p.OracleBlock]`
//
// these will be briefly unlocked during data collection period, at the end of
// the function call, these will be locked
func (o *Oracle) handleBroadcastPriceTickInterval(sig blockTickSignal) {
	o.logger.Debug("broadcast price block tick.")

	defer func() {
		o.priceOracle.AvgPriceMap.Lock()
		o.priceOracle.AvgPriceMap.Clear()
		o.priceOracle.AvgPriceMap.Unlock()

		o.broadcastPricePoints.Clear()
		o.broadcastPriceSig.Clear()
		o.broadcastPriceBlocks.Clear()
	}()

	// broadcast local average price
	localAvgPrices := o.priceOracle.AvgPriceMap.GetAveragePrices()

	if err := o.BroadcastMessage(p2p.MsgPriceBroadcast, localAvgPrices); err != nil {
		o.logger.Error("failed to broadcast local average price", "err", err)
		return
	}

	if !sig.isWitness && !sig.isBlockProducer {
		return
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

	if err != nil {
		o.logger.Error(
			"error on broadcast price tick interval",
			"err", err,
			"isProducer", sig.isBlockProducer,
			"isWitness", sig.isWitness,
		)
	}
}

func (o *Oracle) getMedianPricePoint(
	localAvgPrices map[string]p2p.AveragePricePoint,
) map[string]pricePoint {
	o.logger.Debug("collecting average prices")

	// room for network latency
	o.broadcastPricePoints.UnlockTimeout(10 * time.Second)

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
	broadcastedPricePoints := o.broadcastPricePoints.Get()

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

// price block producer

type priceBlockProducer struct{ *Oracle }

type aggregatedPricePoints struct {
	prices  []float64
	volumes []float64
}

func (p *priceBlockProducer) handleSignal(
	sig *blockTickSignal,
	medianPricePoints map[string]pricePoint,
) error {
	// make block with median price + broadcast
	p.logger.Debug("broadcasting new oracle block with median prices")

	block, err := p2p.MakeOracleBlock(
		p.conf.Get().HiveUsername,
		p.conf.Get().HiveActiveKey,
		medianPricePoints,
	)
	if err != nil {
		return err
	}

	if err := p.BroadcastMessage(p2p.MsgPriceBlock, *block); err != nil {
		return err
	}

	// collecting signature and submit to contract
	p.logger.Debug("collecting signature", "block-id", block.ID)

	// room for network latency
	p.broadcastPriceSig.UnlockTimeout(10 * time.Second)

	sigThreshold := int(math.Ceil(float64(len(sig.electedMembers)) * 2.0 / 3.0))
	block.Signatures = make([]string, 0, sigThreshold)

	signedBlocks := p.broadcastPriceSig.Slice()
	for i := range signedBlocks {
		signedBlock := &signedBlocks[i]
		if block.ID != signedBlock.ID {
			p.logger.Debug(
				"invalid block id, rejecting",
				"block-id", signedBlock.ID,
			)
			continue
		}

		if p.validateSignedBlock(signedBlock) {
			block.Signatures = append(
				block.Signatures,
				signedBlock.Signatures[0],
			)
		}
	}

	if len(block.Signatures) < sigThreshold {
		return errors.New("failed to meet signature threshold")
	}

	return p.submitToContract(block)
}

func (p *priceBlockProducer) validateSignedBlock(block *p2p.OracleBlock) bool {
	if len(block.Signatures) != 1 {
		return false
	}

	/*
		if err := v.Struct(block); err != nil {
			return false
		}
	*/

	// TODO: validate signature
	return true
}

// price block witness

var errBlockExpired = errors.New("block expired")

type priceBlockWitness struct{ *Oracle }

func (p *priceBlockWitness) handleSignal(
	sig *blockTickSignal,
	medianPricePoints map[string]pricePoint,
) error {
	// room for network latency
	p.broadcastPriceBlocks.UnlockTimeout(10 * time.Second)

	blocks := p.broadcastPriceBlocks.Slice()

	for _, block := range blocks {
		ok := p.validateBlock(&block, medianPricePoints)
		if !ok {
			p.logger.Debug(
				"block rejected",
				"block-id", block.ID,
				"block-data", block.Data,
			)
			continue
		}

		sig, err := p.signBlock(&block)
		if err != nil {
			return err
		}

		block.Signatures = append(block.Signatures, sig)

		if err := p.BroadcastMessage(p2p.MsgPriceSignature, block); err != nil {
			return err
		}
	}

	return nil
}

// TODO: what do i need to validate?
func (p *priceBlockWitness) validateBlock(
	block *p2p.OracleBlock,
	localMedianPrice map[string]pricePoint,
) bool {
	broadcastedMedianPrices := make(map[string]pricePoint)
	if err := json.Unmarshal(block.Data, &broadcastedMedianPrices); err != nil {
		return false
	}

	for sym, pricePoint := range localMedianPrice {
		bpp, ok := broadcastedMedianPrices[sym]
		if !ok {
			return false
		}

		var (
			priceOk  = float64Eq(pricePoint.price, bpp.price)
			volumeOk = float64Eq(pricePoint.volume, bpp.volume)
		)

		if !priceOk || !volumeOk {
			return false
		}
	}

	return false
}

func (p *priceBlockWitness) signBlock(b *p2p.OracleBlock) (string, error) {
	timeThreshold := time.Now().UTC().Add(-20 * time.Second)

	blockExpired := timeThreshold.After(b.TimeStamp)
	if blockExpired {
		return "", errBlockExpired
	}

	// TODO: implement block verification + signing
	return "signature", nil
}
