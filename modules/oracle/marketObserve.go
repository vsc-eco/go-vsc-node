package oracle

import (
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

		case sig := <-o.broadcastPriceSignal:
			// broadcast local average price
			localAvgPrices := o.priceOracle.AvgPriceMap.GetAveragePrices()
			o.BroadcastMessage(&p2p.OracleMessage{
				Type: p2p.MsgPriceBroadcast,
				Data: localAvgPrices,
			})

			if sig.isBlockProducer {
				o.pollMedianPriceSignature(sig, localAvgPrices)
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

func (o *Oracle) handleBroadcastSignal(sig blockTickSignal) {

	if !sig.isWitness && !sig.isBlockProducer {
		return
	}

	/*
		if sig.isBlockProducer {
			ts := time.Now().UTC().Unix()
			for i := range medianPriceBuf {
				medianPriceBuf[i].UnixTimeStamp = ts
			}

			// room for network latency
			medianPricePoints := makeMedianPrices(medianPriceBuf)
			nodeId := o.conf.Get()
			vscBlock, err := p2p.MakeVscBlock(
				nodeId.HiveUsername,
				nodeId.HiveActiveKey,
				medianPricePoints,
			)
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
	*/
}

// TODO: reimplement
func (o *Oracle) pollMedianPriceSignature(
	sig blockTickSignal,
	localAvgPrices map[string]p2p.AveragePricePoint,
) error {
	/*
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
