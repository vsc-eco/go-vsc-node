package oracle

import (
	"fmt"
	"log"
	"time"
	"vsc-node/modules/oracle/p2p"

	"github.com/go-playground/validator/v10"
)

var (
	v = validator.New(validator.WithRequiredStructEnabled())
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
			if err := o.handlePriceTickInterval(sig); err != nil {
				log.Println(
					"[oracle] error on broadcastPriceTick interval.",
					err,
				)
			}
		}
	}
}

func (o *Oracle) handlePriceTickInterval(sig blockTickSignal) error {
	defer func() {
		o.priceOracle.AvgPriceMap.Clear()
		o.broadcastPricePoints.Clear()
		o.broadcastPriceSig.Clear()
		o.broadcastPriceBlocks.Clear()
	}()

	// broadcast local average price
	localAvgPrices := o.priceOracle.AvgPriceMap.GetAveragePrices()
	o.BroadcastMessage(&p2p.OracleMessage{
		Type: p2p.MsgPriceBroadcast,
		Data: localAvgPrices,
	})

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

func (o *Oracle) submitToContract(data any) {
	fmt.Println("not implemented")
}
