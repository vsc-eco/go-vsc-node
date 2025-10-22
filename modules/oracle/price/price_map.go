package price

import (
	"strings"
	"sync"
)

type PriceMap struct {
	buf map[string]AveragePricePoint
	mtx *sync.Mutex
}

type PricePoint struct {
	Price  float64 `json:"price"`
	Volume float64 `json:"volume"`
}

type AveragePricePoint struct {
	PricePoint
	counter uint32
}

func MakePriceMap() *PriceMap {
	return &PriceMap{
		buf: map[string]AveragePricePoint{},
		mtx: &sync.Mutex{},
	}
}

func (pm *PriceMap) Observe(pricePoints map[string]PricePoint) {
	pm.mtx.Lock()
	defer pm.mtx.Unlock()

	for symbol, pricePoint := range pricePoints {
		symbol = strings.ToLower(symbol)

		p, ok := pm.buf[symbol]
		if !ok {
			p = AveragePricePoint{
				PricePoint: pricePoint,
				counter:    0,
			}
		} else {
			nextAvgVolume := calcNextAvg(p.Volume, pricePoint.Volume, p.counter)
			nextAvgPrice := calcNextAvg(p.Price, pricePoint.Price, p.counter)
			nextCounter := p.counter + 1

			p = AveragePricePoint{
				PricePoint: PricePoint{
					Price:  nextAvgPrice,
					Volume: nextAvgVolume,
				},
				counter: nextCounter,
			}
		}

		pm.buf[symbol] = p
	}
}

func (pm *PriceMap) Clear() {
	pm.mtx.Lock()
	defer pm.mtx.Unlock()
	pm.buf = make(map[string]AveragePricePoint)
}

func (pm *PriceMap) GetAveragePricePoints() map[string]PricePoint {
	pm.mtx.Lock()
	defer pm.mtx.Unlock()

	buf := make(map[string]PricePoint, len(pm.buf))
	for symbol, pricePoint := range pm.buf {
		buf[symbol] = pricePoint.PricePoint
	}

	return buf
}

func calcNextAvg(
	currentAverage, newValue float64,
	currentCounter uint32,
) float64 {
	currentTotal := currentAverage * float64(currentCounter)
	newTotal := currentTotal + newValue
	return newTotal / float64(currentCounter+1)
}
