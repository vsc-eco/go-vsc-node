package oracle

import (
	"encoding/json"
	"fmt"
	"log"
	"time"
	"vsc-node/modules/oracle/p2p"
)

func (o *Oracle) marketObserve() {
	var (
		pricePollTicker = time.NewTicker(priceOraclePollInterval)
	)

	for {
		select {
		case <-o.ctx.Done():
			return

		case <-pricePollTicker.C:
			for _, api := range o.priceOracle.PriceAPIs {
				go api.QueryMarketPrice(watchSymbols, o.observePriceChan)
			}

		case broadcastSignal := <-o.broadcastPriceSignal:
			fmt.Println(broadcastSignal)
			o.broadcastPricePoints()
			isBlockProducer := true
			if isBlockProducer {
				vscBlock, err := o.broadcastMedianPrice()
				if err != nil {
					log.Println(err)
					continue
				}
				o.pollMedianPriceSignature(vscBlock)
			}

		case blockRelaySignal := <-o.blockRelaySignal:
			fmt.Println(blockRelaySignal)
			go o.relayBtcHeadBlock()

		case msg := <-o.msgChan:
			// TODO: which one to send broadcast within the network?
			if err := o.service.Send(msg); err != nil {
				log.Println("[oracle] failed broadcast message", msg, err)
				continue
			}

			jbytes, err := json.Marshal(msg)
			if err != nil {
				log.Println("[oracle] failed serialize message", msg, err)
				continue
			}
			o.p2p.SendToAll(p2p.OracleTopic, jbytes)

		case pricePoints := <-o.observePriceChan:
			o.priceOracle.ObservePricePoint(pricePoints)

		case btcHeadBlock := <-o.blockRelayChan:
			fmt.Println("TODO: validate btcHeadBlock", btcHeadBlock)

		}
	}
}
