package price

import (
	"strings"
	"sync"
	"vsc-node/modules/oracle/p2p"
)

type priceMap struct {
	buf map[string]pricePointData
	mtx *sync.Mutex
}

type pricePointData struct {
	avgPrice  float64
	avgVolume float64
	counter   uint32
}

func makePriceMap() priceMap {
	return priceMap{
		buf: make(map[string]pricePointData),
		mtx: new(sync.Mutex),
	}
}

func (pm *priceMap) Observe(pricePoints map[string]p2p.ObservePricePoint) {
	pm.mtx.Lock()
	defer pm.mtx.Unlock()

	for priceSymbol, pricePoint := range pricePoints {
		symbol := strings.ToUpper(priceSymbol)
		avg, ok := pm.buf[symbol]

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

		pm.buf[symbol] = avg
	}
}

func (pm *priceMap) Flush() {
	pm.mtx.Lock()
	defer pm.mtx.Unlock()

	pm.buf = make(map[string]pricePointData)
}

func (pm *priceMap) GetAveragePrices() map[string]p2p.AveragePricePoint {
	pm.mtx.Lock()
	defer pm.mtx.Unlock()

	out := make(map[string]p2p.AveragePricePoint)

	for symbol, p := range pm.buf {
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
