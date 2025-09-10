package oracle

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"
	"vsc-node/modules/oracle/p2p"
)

var (
	pricePollTicker   = time.NewTicker(priceOraclePollInterval)
	broadcastPriceBuf = make([]p2p.AveragePricePoint, 0, 256)
)

func (o *Oracle) marketObserve() {

	for {
		select {
		case <-o.ctx.Done():
			return

		case <-pricePollTicker.C:
			for _, api := range o.priceOracle.PriceAPIs {
				go api.QueryMarketPrice(watchSymbols, o.observePriceChan)
			}

		case broadcastSignal := <-o.broadcastPriceSignal:
			o.handleBroadcastSignal(broadcastSignal)
			broadcastPriceBuf = broadcastPriceBuf[:0]
			o.priceOracle.ResetPriceCache()

		case avgPricePoints := <-o.broadcastPriceChan:
			now := time.Now().UTC().Unix()
			for i := range avgPricePoints {
				avgPricePoints[i].UnixTimeStamp = now
			}
			broadcastPriceBuf = append(broadcastPriceBuf, avgPricePoints...)

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

func (o *Oracle) handleBroadcastSignal(sig blockTickSignal) {
	// local price
	medianPriceBuf := make([]p2p.AveragePricePoint, 0, len(watchSymbols))

	for _, symbol := range watchSymbols {
		avgPricePoint, err := o.priceOracle.GetAveragePrice(symbol)
		if err != nil {
			log.Println("symbol not found in map:", symbol)
			continue
		}
		medianPriceBuf = append(medianPriceBuf, *avgPricePoint)
	}

	o.msgChan <- &p2p.OracleMessage{
		Type: p2p.MsgPriceOracleBroadcast,
		Data: medianPriceBuf,
	}

	if !sig.isWitness && !sig.isBlockProducer {
		return
	}

	if sig.isBlockProducer {
		ts := time.Now().UTC().Unix()
		for i := range medianPriceBuf {
			medianPriceBuf[i].UnixTimeStamp = ts
		}

		// room for network latency
		ctx, cancel := context.WithTimeout(context.Background(), listenDuration)
		defer cancel()
		<-ctx.Done()

		medianPricePoints := makeMedianPrices(medianPriceBuf)
		vscBlock := p2p.VSCBlock{
			Data:       medianPricePoints,
			Signatures: []string{},
		}

		o.msgChan <- &p2p.OracleMessage{
			Type: p2p.MsgPriceOracleNewBlock,
			Data: vscBlock,
		}

		o.pollMedianPriceSignature(vscBlock, sig)
	} else if sig.isWitness {
	}
}

func (o *Oracle) pollMedianPriceSignature(
	block p2p.VSCBlock,
	sig blockTickSignal,
) {
	/*
		// collect signatures
		// TODO: how do i get the latest witnesses?
		witnesses, err := o.witness.GetLastestWitnesses()
		if err != nil {
			log.Println("failed to get latest witnesses", err)
			return
		}
		log.Println(witnesses)
	*/

	// TODO: validate 2/3 of witness signatures
	o.msgChan <- &p2p.OracleMessage{
		Type: p2p.MsgPriceOracleSignedBlock,
		Data: block,
	}
}
