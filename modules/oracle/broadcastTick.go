package oracle

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"math"
	"strings"
	"time"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/oracle/p2p"
	"vsc-node/modules/oracle/price"
	"vsc-node/modules/oracle/threadsafe"
)

type blockTickSignal struct {
	isBlockProducer bool
	isWitness       bool
	electedMembers  []elections.ElectionMember
}

func (o *Oracle) handleBroadcastPriceTickInterval(sig blockTickSignal) {
	o.logger.Debug("broadcast price block tick.")

	defer o.priceOracle.ClearPriceCache()

	// broadcast local average price
	localAvgPrices := o.priceOracle.GetLocalQueriedAveragePrices()
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
) map[string]price.PricePoint {
	o.logger.Debug("collecting average prices")

	// updating with local price points
	appBuf := make(map[string]aggregatedPricePoints)
	for k, v := range localAvgPrices {
		sym := strings.ToUpper(k)
		appBuf[sym] = aggregatedPricePoints{
			prices:  []float64{v.Price},
			volumes: []float64{v.Volume},
		}
	}

	// collect broadcastPriceBlocks
	timeThreshold := time.Now().UTC().Add(-time.Hour)
	collector := pricePointCollector(appBuf, &timeThreshold)

	// room for network latency
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	o.priceOracle.PricePoints.Collect(ctx, collector)

	// calculating the median volumes + prices
	medianPricePoint := make(map[string]price.PricePoint)
	for sym, app := range appBuf {
		medianPricePoint[sym] = price.PricePoint{
			Price:  getMedian(app.prices),
			Volume: getMedian(app.volumes),
			// peerID:      "",
			CollectedAt: time.Now().UTC(),
		}
	}

	return medianPricePoint
}

func pricePointCollector(
	appBuf map[string]aggregatedPricePoints,
	timeThreshold *time.Time,
) threadsafe.CollectFunc[price.PricePointMap] {
	const earlyReturn = false

	return func(ppm price.PricePointMap) bool {
		for sym, pricePoints := range ppm {
			sym = strings.ToUpper(sym)

			for _, pp := range pricePoints {
				v, ok := appBuf[sym]
				if !ok {
					log.Println("unsupported symbol", sym)
				}

				pricePointExpired := timeThreshold.After(pp.CollectedAt)
				if pricePointExpired {
					continue
				}

				v.volumes = append(appBuf[sym].volumes, pp.Volume)
				v.prices = append(appBuf[sym].prices, pp.Price)

				appBuf[sym] = v
			}
		}

		return earlyReturn
	}
}

// price block producer

type priceBlockProducer struct{ *Oracle }

type aggregatedPricePoints struct {
	prices  []float64
	volumes []float64
}

func (p *priceBlockProducer) handleSignal(
	sig *blockTickSignal,
	medianPricePoints map[string]price.PricePoint,
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

	sigThreshold := int(math.Ceil(float64(len(sig.electedMembers)) * 2.0 / 3.0))
	block.Signatures = make([]string, 0, sigThreshold)

	// room for network latency
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	p.priceOracle.SignedBlocks.Collect(ctx, func(ob p2p.OracleBlock) bool {
		if err := p.validateSignedBlock(&ob); err != nil {
			p.logger.Error("failed to validate block signature")
			return false
		}

		block.Signatures = append(block.Signatures, ob.Signatures[0])
		return len(block.Signatures) >= sigThreshold
	})

	if len(block.Signatures) < sigThreshold {
		return errors.New("failed to meet signature threshold")
	}

	return p.submitToContract(block)
}

func (p *priceBlockProducer) validateSignedBlock(block *p2p.OracleBlock) error {
	if len(block.Signatures) != 1 {
		return errors.New("missing signature")
	}

	/*
		if err := v.Struct(block); err != nil {
			return false
		}
	*/

	// TODO: validate signature
	return nil
}

// price block witness

var errBlockExpired = errors.New("block expired")

type priceBlockWitness struct{ *Oracle }

func (p *priceBlockWitness) handleSignal(
	sig *blockTickSignal,
	medianPricePoints map[string]price.PricePoint,
) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	blocks := make([]p2p.OracleBlock, 0, 2)

	// room for network latency
	p.priceOracle.ProducerBlocks.Collect(ctx, func(ob p2p.OracleBlock) bool {
		blocks = append(blocks, ob)
		return false
	})

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
	localMedianPrice map[string]price.PricePoint,
) bool {
	broadcastedMedianPrices := make(map[string]price.PricePoint)
	if err := json.Unmarshal(block.Data, &broadcastedMedianPrices); err != nil {
		return false
	}

	for sym, pricePoint := range localMedianPrice {
		bpp, ok := broadcastedMedianPrices[sym]
		if !ok {
			return false
		}

		var (
			priceOk  = float64Eq(pricePoint.Price, bpp.Price)
			volumeOk = float64Eq(pricePoint.Volume, bpp.Volume)
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
