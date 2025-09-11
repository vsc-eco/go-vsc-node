package oracle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"time"
	"vsc-node/modules/oracle/p2p"

	"github.com/go-playground/validator/v10"
)

var (
	v               = validator.New(validator.WithRequiredStructEnabled())
	pricePollTicker = time.NewTicker(priceOraclePollInterval)

	// to be processed by the block producer, calculating median price
	// and creating new block
	broadcastPriceBuf = make([]p2p.AveragePricePoint, 0, 256)

	// to be signed by the witness
	newBlockBuf = make([]p2p.VSCBlock, 0, 256)
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
			newBlockBuf = newBlockBuf[:0]
			o.priceOracle.ResetPriceCache()

		case newBlock := <-o.broadcastPriceBlockChan:
			newBlockBuf = append(newBlockBuf, newBlock)

		case avgPricePoints := <-o.broadcastPriceChan:
			now := time.Now().UTC().Unix()
			for i := range avgPricePoints {
				avgPricePoints[i].UnixTimeStamp = now
			}
			broadcastPriceBuf = append(broadcastPriceBuf, avgPricePoints...)

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
		vscBlock, err := p2p.MakeVscBlock(medianPricePoints)
		if err != nil {
			log.Println("[oracle] failed to make new vsc block", err)
			return
		}
		o.msgChan <- &p2p.OracleMessage{
			Type: p2p.MsgPriceOracleNewBlock,
			Data: *vscBlock,
		}

		o.pollMedianPriceSignature(*vscBlock, sig)
	} else if sig.isWitness {
		timeThreshold := time.Now().UTC().UnixMilli()

		for _, block := range newBlockBuf {
			if block.TimeStamp < timeThreshold {
				continue
			}

			// TODO: sign and broadcast
			fmt.Println(block)
		}
	}
}

func (o *Oracle) pollMedianPriceSignature(
	block p2p.VSCBlock,
	sig blockTickSignal,
) error {

	// poll signatures for 10 seconds
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sigThreshold := int(math.Ceil(float64(len(sig.electedMembers) * 2 / 3)))
	block.Signatures = make([]string, 0, sigThreshold)

	sigCount := 0

	for sigCount < sigThreshold {
		select {
		case <-ctx.Done():
			return errors.New("operation timed out")

		case signedBlock := <-o.priceBlockSignatureReceiver:
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

	// TODO: submit block to contract
	o.msgChan <- &p2p.OracleMessage{
		Type: p2p.MsgPriceOracleSignedBlock,
		Data: block,
	}

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
