package price

import (
	"encoding/json"
	"errors"
	"slices"
	"time"
)

var (
	errSymbolNotFound = errors.New("symbol not found")
)

type priceMap struct{ priceSymbolMap }
type priceSymbolMap map[string][]observePricePoint
type observePricePoint struct {
	symbol string
	price  float64
	volume float64
}

func (o *observePricePoint) String() string {
	buf := map[string]any{
		"symbol": o.symbol,
		"price":  o.price,
		"volume": o.volume,
	}
	jbytes, _ := json.MarshalIndent(buf, "", "  ")
	return string(jbytes)
}

func makePriceMap() priceMap {
	return priceMap{make(priceSymbolMap)}
}

func (pm *priceMap) getAveragePrice(symbol string) (*AveragePricePoint, error) {
	pricePoints, ok := pm.priceSymbolMap[symbol]
	if !ok {
		return nil, errSymbolNotFound
	}

	// median price
	slices.SortFunc(pricePoints, cmpObservePricePoint)
	var (
		medianIndex        = len(pricePoints) / 2
		medianPrice        = pricePoints[medianIndex].price
		evenNumPricePoints = len(pricePoints)&1 == 0
	)

	if evenNumPricePoints {
		medianPrice = (medianPrice + pricePoints[medianIndex-1].price) / 2
	}

	// average price + volume
	var (
		priceSum  = float64(0)
		volumeSum = float64(0)
	)

	for _, pricePoint := range pricePoints {
		priceSum += pricePoint.price
		volumeSum += pricePoint.volume
	}

	out := &AveragePricePoint{
		Symbol:        symbol,
		MedianPrice:   medianPrice,
		Price:         priceSum / float64(len(pricePoints)),
		Volume:        volumeSum / float64(len(pricePoints)),
		UnixTimeStamp: time.Now().UTC().UnixMilli(),
	}

	return out, nil
}

// compare function argument to slices.SortFunc to sort observePricePoint
func cmpObservePricePoint(a observePricePoint, b observePricePoint) int {
	if a.price < b.price {
		return -1
	}
	return 1
}

func (pm *priceMap) observe(pricePoint observePricePoint) {
	avg, ok := pm.priceSymbolMap[pricePoint.symbol]

	if !ok {
		avg = make([]observePricePoint, 0, 32)
	}

	avg = append(avg, pricePoint)
	pm.priceSymbolMap[pricePoint.symbol] = avg
}
