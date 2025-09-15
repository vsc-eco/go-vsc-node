package oracle

import (
	"context"
	"errors"
	"log"
	"math"
	"strings"
	"time"
	"vsc-node/modules/oracle/p2p"

	"github.com/go-playground/validator/v10"
)

var (
	v = validator.New(validator.WithRequiredStructEnabled())

	// to be signed by the witness
	newBlockBuf = make([]p2p.VSCBlock, 0, 256)
)

func (o *Oracle) marketObserve() {
	pricePollTicker := time.NewTicker(priceOraclePollInterval)

	for {
		select {
		case <-o.ctx.Done():
			return

		case <-pricePollTicker.C:
			for _, api := range o.priceOracle.PriceAPIs {
				go func() {
					pricePoints, err := api.QueryMarketPrice(watchSymbols)
					if err != nil {
						log.Println("failed to query for market price:", err)
						return
					}
					o.priceOracle.AvgPriceMap.Observe(pricePoints)
				}()

			}

		case sig := <-o.broadcastPriceTick:
			// broadcast local average price
			localAvgPrices := o.priceOracle.AvgPriceMap.GetAveragePrices()
			o.BroadcastMessage(&p2p.OracleMessage{
				Type: p2p.MsgPriceBroadcast,
				Data: localAvgPrices,
			})

			if sig.isBlockProducer {
				o.blockProducerHandler(&sig, localAvgPrices)
			} else if sig.isWitness {
			}
			o.priceOracle.AvgPriceMap.Flush()

			/*
				o.handleBroadcastSignal(broadcastSignal)
				broadcastPriceBuf = broadcastPriceBuf[:0]
				newBlockBuf = newBlockBuf[:0]
			*/

		case newBlock := <-o.broadcastPriceBlockChan:
			// TODO: move this channel to witness processing
			newBlockBuf = append(newBlockBuf, newBlock)

			/*
				case btcHeadBlock := <-o.blockRelayChan:
					fmt.Println("TODO: validate btcHeadBlock", btcHeadBlock)

				case blockRelaySignal := <-o.blockRelaySignal:
					fmt.Println(blockRelaySignal)
					go o.relayBtcHeadBlock()
			*/

		}
	}
}

func (o *Oracle) blockProducerHandler(
	sig *blockTickSignal,
	localAvgPrices map[string]p2p.AveragePricePoint,
) error {
	medianPricePoints := o.getMedianPricePoint(localAvgPrices)

	block, err := p2p.MakeVscBlock(
		o.conf.Get().HiveUsername,
		o.conf.Get().HiveActiveKey,
		medianPricePoints,
	)
	if err != nil {
		return err
	}

	msg := p2p.OracleMessage{Type: p2p.MsgPriceOracleNewBlock, Data: *block}
	if err := o.BroadcastMessage(&msg); err != nil {
		return err
	}

	return o.pollMedianPriceSignature(sig, block)
}

func (o *Oracle) getMedianPricePoint(
	localAvgPrices map[string]p2p.AveragePricePoint,
) map[string]pricePoint {
	// room for network latency
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

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
	broadcastedPricePoints := o.broadcastPricePoints.GetMap()

	for sym, pricePoints := range broadcastedPricePoints {
		sym = strings.ToUpper(sym)

		for _, pricePoint := range pricePoints {
			v, ok := appBuf[sym]
			if !ok {
				log.Println("unsupported symbol", sym)
			}

			if pricePoint.collectedAt.Compare(timeThreshold) == 1 {
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

// TODO: reimplement
func (o *Oracle) pollMedianPriceSignature(
	sig *blockTickSignal,
	block *p2p.VSCBlock,
) error {
	sigThreshold := int(math.Ceil(float64(len(sig.electedMembers) * 2 / 3)))
	block.Signatures = make([]string, 0, sigThreshold)

	sigCount := 0

	// poll signatures for 10 seconds
	ctx, cancel := context.WithTimeout(context.Background(), listenDuration)
	defer cancel()

	for sigCount < sigThreshold {
		select {
		case <-ctx.Done():
			return errors.New("operation timed out")

		case signedBlock := <-o.priceBlockSignatureChan:
			if signedBlock.ID != block.ID {
				continue
			}

			if !validateSignedBlock(&signedBlock) {
				continue
			}

			block.Signatures = append(
				block.Signatures,
				signedBlock.Signatures[0],
			)
			sigCount += 1
		}

	}

	/*
		// TODO: submit block to contract
		o.msgChan <- &p2p.OracleMessage{
			Type: p2p.MsgPriceOracleSignedBlock,
			Data: block,
		}
	*/

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
