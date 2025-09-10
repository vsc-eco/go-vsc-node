package price

import (
	"errors"
	"strings"
	"vsc-node/modules/oracle/p2p"
)

var (
	errSymbolNotFound = errors.New("symbol not found")
)

type priceMap struct{ priceSymbolMap }
type priceSymbolMap map[string]pricePointData
type pricePointData struct {
	avgPrice  float64
	avgVolume float64
	counter   uint32
}

func makePriceMap() priceMap {
	return priceMap{make(priceSymbolMap)}
}

func (pm *priceMap) getAveragePrice(
	symbol string,
) (*p2p.AveragePricePoint, error) {
	symbol = strings.ToUpper(symbol)

	p, ok := pm.priceSymbolMap[symbol]
	if !ok {
		return nil, errSymbolNotFound
	}

	out := p2p.MakeAveragePricePoint(symbol, p.avgPrice, p.avgVolume)

	return &out, nil
}

func (pm *priceMap) observe(pricePoint p2p.ObservePricePoint) {
	symbol := strings.ToUpper(pricePoint.Symbol)
	avg, ok := pm.priceSymbolMap[symbol]

	if !ok {
		avg = pricePointData{
			avgPrice:  pricePoint.Price,
			avgVolume: pricePoint.Volume,
			counter:   1,
		}
	} else {
		avg.avgPrice = calcNextAvg(avg.avgPrice, pricePoint.Price, avg.counter)
		avg.avgVolume = calcNextAvg(avg.avgVolume, pricePoint.Volume, avg.counter)
		avg.counter += 1
	}

	pm.priceSymbolMap[symbol] = avg
}

func calcNextAvg(
	currentAverage, newValue float64,
	currentCounter uint32,
) float64 {
	currentTotal := currentAverage * float64(currentCounter)
	newTotal := currentTotal + newValue
	return newTotal / float64(currentCounter+1)
}
