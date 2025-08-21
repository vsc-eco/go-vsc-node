package price

import "errors"

var (
	errSymbolNotFound = errors.New("symbol not found")
)

type priceMap struct{ priceSymbolMap }
type priceSymbolMap map[string]avgPricePoint
type avgPricePoint struct {
	average     float64
	medianPrice float64
	volume      float64 // TODO: keep track of this
	counter     uint64
}

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

// query the price over the hour and drop off things outside the hour mark
// at the hour mark, broadcast the price
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
