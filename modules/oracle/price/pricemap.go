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
type priceSymbolMap map[string][]ObservePricePoint

type ObservePricePoint struct {
	Symbol string  `json:"symbol,omitempty"`
	Price  float64 `json:"price,omitempty"`
	Volume float64 `json:"volume,omitempty"`
}

func (o *ObservePricePoint) String() string {
	buf := map[string]any{
		"symbol": o.Symbol,
		"price":  o.Price,
		"volume": o.Volume,
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
func cmpObservePricePoint(a ObservePricePoint, b ObservePricePoint) int {
	if a.Price < b.Price {
		return -1
	}
	return 1
}

func (pm *priceMap) observe(pricePoint ObservePricePoint) {
	avg, ok := pm.priceSymbolMap[pricePoint.Symbol]

	if !ok {
		avg = make([]ObservePricePoint, 0, 32)
	}

	avg = append(avg, pricePoint)
	pm.priceSymbolMap[pricePoint.Symbol] = avg
}
