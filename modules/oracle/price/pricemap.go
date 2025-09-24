package price

import (
	"strings"
	"vsc-node/modules/oracle/p2p"
	"vsc-node/modules/oracle/threadsafe"
)

type priceMap struct {
	*threadsafe.Map[string, pricePointData]
}

type pricePointData struct {
	avgPrice  float64
	avgVolume float64
	counter   uint32
}

func makePriceMap() priceMap {
	tsMap := threadsafe.NewMap[string, pricePointData]()
	tsMap.Unlock()

	return priceMap{tsMap}
}

func (pm *priceMap) Observe(pricePoints map[string]p2p.ObservePricePoint) {
	const threadBlocking = true

	pm.Update(func(m map[string]pricePointData) {
		for priceSymbol, pricePoint := range pricePoints {
			symbol := strings.ToUpper(priceSymbol)
			avg, ok := m[symbol]

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

			m[symbol] = avg
		}
	})
}

func (pm *priceMap) GetAveragePrices() map[string]p2p.AveragePricePoint {
	out := make(map[string]p2p.AveragePricePoint)

	for symbol, p := range pm.Get() {
		symbol = strings.ToUpper(symbol)
		out[symbol] = p2p.MakeAveragePricePoint(p.avgPrice, p.avgVolume)
	}

	return out
}

func calcNextAvg(
	currentAverage, newValue float64,
	currentCounter uint32,
) float64 {
	currentTotal := currentAverage * float64(currentCounter)
	newTotal := currentTotal + newValue
	return newTotal / float64(currentCounter+1)
}
