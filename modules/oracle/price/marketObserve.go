package price

import (
	"sync"
	"time"
)

func (p *PriceOracle) marketObserve() {
	pricePollTicker := time.NewTicker(p.pricePollInterval)

	for {
		select {
		case <-p.ctx.Done():
			return

		case <-pricePollTicker.C:
			p.queryMarket()
		}
	}
}

func (p *PriceOracle) queryMarket() {
	wg := &sync.WaitGroup{}
	wg.Add(len(p.priceAPIs))

	for src, api := range p.priceAPIs {
		go func(src string, api priceQuery) {
			defer wg.Done()

			pricePoints, err := api.queryMarketPrice(p.watchSymbols)
			if err != nil {
				p.logger.Error("failed to query market", "src", src, "err", err)
				return
			}

			p.avgPriceMap.Observe(pricePoints)
			p.logger.Debug("market price fetched", "src", src)
		}(src, api)
	}

	wg.Wait()
}
