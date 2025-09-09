package oracle

import (
	"log"
	"vsc-node/modules/oracle/p2p"
)

// returns nil if the node is not the block producer
func (o *Oracle) broadcastPricePoints() {
	defer o.priceOracle.ResetPriceCache()

	// local price

	buf := make([]p2p.AveragePricePoint, 0, len(watchSymbols))

	for _, symbol := range watchSymbols {
		avgPricePoint, err := o.priceOracle.GetAveragePrice(symbol)
		if err != nil {
			log.Println("symbol not found in map:", symbol)
			continue
		}
		buf = append(buf, *avgPricePoint)
	}

	o.broadcastPriceChan <- buf

	o.msgChan <- &p2p.OracleMessage{
		Type: p2p.MsgPriceOracleBroadcast,
		Data: buf,
	}

	/*
		if node is not a block producer {
			- calculate the median of each symbols
			- compare with the broadcasted block
			- sign and broadcast the signature
			return nil
		}
	*/

}
