package price

import "errors"

var (
	errSymbolNotFound = errors.New("symbol not found")
)

type (
	priceMap       struct{ priceSymbolMap }
	priceSymbolMap map[string]avgPricePoint
	avgPricePoint  struct {
		average float64
		counter uint64
	}
)

func makePriceMap() priceMap {
	return priceMap{make(priceSymbolMap)}
}

func (pm *priceMap) getAveragePrice(symbol string) (float64, error) {
	price, ok := pm.priceSymbolMap[symbol]
	if !ok {
		return 0.0, errSymbolNotFound
	}

	return price.average, nil
}

func (pm *priceMap) observe(pricePoint PricePoint) {
	avg, ok := pm.priceSymbolMap[pricePoint.Symbol]

	if !ok {
		pm.priceSymbolMap[pricePoint.Symbol] = avgPricePoint{
			average: pricePoint.Price,
			counter: 1,
		}
		return
	}

	var (
		nextSum = (avg.average * float64(avg.counter)) + pricePoint.Price
		counter = avg.counter + 1
	)

	pm.priceSymbolMap[pricePoint.Symbol] = avgPricePoint{
		average: nextSum / float64(counter),
		counter: counter,
	}
}
