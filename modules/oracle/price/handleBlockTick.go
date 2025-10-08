package price

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"math"
	"slices"
	"strings"
	"time"
	"vsc-node/modules/oracle/p2p"
	"vsc-node/modules/oracle/threadsafe"
)

const float64Epsilon = 1e-9

// HandleBlockTick implements oracle.BlockTickHandler.
func (o *PriceOracle) HandleBlockTick(
	sig p2p.BlockTickSignal,
	p2pSpec p2p.OracleP2PSpec,
) {
	o.logger.Debug("broadcast price block tick.")

	defer o.ClearPriceCache()

	// broadcast local average price
	localAvgPrices := o.GetLocalQueriedAveragePrices()
	if err := p2pSpec.Broadcast(p2p.MsgPriceBroadcast, localAvgPrices); err != nil {
		o.logger.Error("failed to broadcast local average price", "err", err)
		return
	}

	if !sig.IsWitness && !sig.IsProducer {
		return
	}

	// get median prices
	medianPricePoints := o.getMedianPricePoint(localAvgPrices)

	// make block / sign block
	var signalHandler func(*p2p.BlockTickSignal, map[string]PricePoint) error

	if sig.IsProducer {
		priceBlockProducer := &priceBlockProducer{o, p2pSpec}
		signalHandler = priceBlockProducer.handleSignal
	} else if sig.IsWitness {
		priceBlockWitness := &priceBlockWitness{o, p2pSpec}
		signalHandler = priceBlockWitness.handleSignal
	}

	if err := signalHandler(&sig, medianPricePoints); err != nil {
		o.logger.Error(
			"error on broadcast price tick interval",
			"err", err,
			"isProducer", sig.IsProducer,
			"isWitness", sig.IsWitness,
		)
	}
}

func (o *PriceOracle) getMedianPricePoint(
	localAvgPrices map[string]p2p.AveragePricePoint,
) map[string]PricePoint {
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

	o.pricePoints.Collect(ctx, collector)

	// calculating the median volumes + prices
	medianPricePoint := make(map[string]PricePoint)
	for sym, app := range appBuf {
		medianPricePoint[sym] = PricePoint{
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
) threadsafe.CollectFunc[PricePointMap] {
	const earlyReturn = false

	return func(ppm PricePointMap) bool {
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

type priceBlockProducer struct {
	*PriceOracle
	p2p.OracleP2PSpec
}

type aggregatedPricePoints struct {
	prices  []float64
	volumes []float64
}

func (p *priceBlockProducer) handleSignal(
	sig *p2p.BlockTickSignal,
	medianPricePoints map[string]PricePoint,
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

	if err := p.Broadcast(p2p.MsgPriceBlock, *block); err != nil {
		return err
	}

	// collecting signature and submit to contract
	p.logger.Debug("collecting signature", "block-id", block.ID)

	sigThreshold := int(math.Ceil(float64(len(sig.ElectedMembers)) * 2.0 / 3.0))
	block.Signatures = make([]string, 0, sigThreshold)

	// room for network latency
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err = p.signatures.Collect(ctx, func(ob p2p.OracleBlock) bool {
		if err := p.validateSignedBlock(&ob); err != nil {
			p.logger.Error("failed to validate block signature")
			return false
		}

		block.Signatures = append(block.Signatures, ob.Signatures[0])
		return len(block.Signatures) >= sigThreshold
	})

	if err != nil {
		return errors.New("failed to meet signature threshold")
	}

	return p.SubmitToContract(block)
}

// TODO: implement block validation
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

type priceBlockWitness struct {
	*PriceOracle
	p2p.OracleP2PSpec
}

func (p *priceBlockWitness) handleSignal(
	sig *p2p.BlockTickSignal,
	medianPricePoints map[string]PricePoint,
) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	blocks := make([]p2p.OracleBlock, 0, 2)

	// room for network latency
	p.producerBlocks.Collect(ctx, func(ob p2p.OracleBlock) bool {
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

		block.Signatures = []string{sig}

		if err := p.Broadcast(p2p.MsgPriceSignature, block); err != nil {
			return err
		}
	}

	return nil
}

// TODO: what do i need to validate?
func (p *priceBlockWitness) validateBlock(
	block *p2p.OracleBlock,
	localMedianPrice map[string]PricePoint,
) bool {
	broadcastedMedianPrices := make(map[string]PricePoint)
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

func float64Eq(a, b float64) bool {
	return math.Abs(a-b) < float64Epsilon
}

func getMedian(buf []float64) float64 {
	if len(buf) == 0 {
		return 0
	}

	slices.Sort(buf)

	evenCount := len(buf)&1 == 0
	if evenCount {
		i := len(buf) / 2
		return (buf[i] + buf[i-1]) / 2
	} else {
		return buf[len(buf)/2]
	}
}
