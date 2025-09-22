package oracle

import (
	"fmt"
	"time"
	"vsc-node/modules/oracle/p2p"
)

func (o *Oracle) marketObserve() {
	pricePollTicker := time.NewTicker(priceOraclePollInterval)
	o.logger.Debug("observing market")

	for {
		select {
		case <-o.ctx.Done():
			return

		case <-pricePollTicker.C:
			o.logger.Debug("pricePollTicker interval ticked")
			for _, api := range o.priceOracle.PriceAPIs {
				go func() {
					pricePoints, err := api.QueryMarketPrice(watchSymbols)
					if err != nil {
						o.logger.Error(
							"failed to query for market price",
							"err", err,
						)
						return
					}
					o.priceOracle.AvgPriceMap.Observe(pricePoints)
				}()
			}
		}
	}
}

func (o *Oracle) submitToContract(data *p2p.OracleBlock) error {
	fmt.Println("not implemented")
	return nil
}
