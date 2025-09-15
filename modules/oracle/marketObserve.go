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
				priceBlockProducer := &priceBlockProducer{o}
				priceBlockProducer.handleSignal(&sig, localAvgPrices)
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

func (p *Oracle) submitToContract(data any) {
	fmt.Println("not implemented")
}
