package price

import (
	"errors"
	"slices"
	"time"
	"vsc-node/modules/oracle/p2p"
)

var (
	errSymbolNotFound = errors.New("symbol not found")
)

type priceMap struct{ priceSymbolMap }
type priceSymbolMap map[string][]p2p.ObservePricePoint

func makePriceMap() priceMap {
	return priceMap{make(priceSymbolMap)}
}

func (pm *priceMap) getAveragePrice(
	symbol string,
) (*p2p.AveragePricePoint, error) {
	pricePoints, ok := pm.priceSymbolMap[symbol]
	if !ok {
		return nil, errSymbolNotFound
	}

	// median price
	slices.SortFunc(pricePoints, cmpObservePricePoint)
	var (
		medianIndex        = len(pricePoints) / 2
		medianPrice        = pricePoints[medianIndex].Price
		evenNumPricePoints = len(pricePoints)&1 == 0
	)

	if evenNumPricePoints {
		medianPrice = (medianPrice + pricePoints[medianIndex-1].Price) / 2
	}

	// average price + volume
	var (
		priceSum  = float64(0)
		volumeSum = float64(0)
	)

	for _, pricePoint := range pricePoints {
		priceSum += pricePoint.Price
		volumeSum += pricePoint.Volume
	}

	out := &p2p.AveragePricePoint{
		Symbol:        symbol,
		MedianPrice:   medianPrice,
		Price:         priceSum / float64(len(pricePoints)),
		Volume:        volumeSum / float64(len(pricePoints)),
		UnixTimeStamp: time.Now().UTC().UnixMilli(),
	}

	return out, nil
}

// compare function argument to slices.SortFunc to sort observePricePoint
func cmpObservePricePoint(
	a p2p.ObservePricePoint,
	b p2p.ObservePricePoint,
) int {
	if a.Price < b.Price {
		return -1
	}
	return 1
}

func (pm *priceMap) observe(pricePoint p2p.ObservePricePoint) {
	avg, ok := pm.priceSymbolMap[pricePoint.Symbol]

	if !ok {
		avg = make([]p2p.ObservePricePoint, 0, 32)
	}

	avg = append(avg, pricePoint)
	pm.priceSymbolMap[pricePoint.Symbol] = avg
}
