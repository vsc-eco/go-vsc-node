package oracle

import (
	"slices"
	"strings"
	"time"
	"vsc-node/modules/oracle/p2p"
)

const (
	listenDuration = time.Second * 10
	hourInSecond   = 3600
)

type pricePoints struct {
	prices  []float64
	volumes []float64
}

type medianPricePointMap = map[string]pricePoints

func makeMedianPrices(
	avgPricePoints []p2p.AveragePricePoint,
) []p2p.AveragePricePoint {
	var (
		timeFrame   = time.Now().UTC().Unix() - hourInSecond
		medPriceMap = make(medianPricePointMap)
	)

	for _, p := range avgPricePoints {
		if p.UnixTimeStamp < timeFrame {
			continue
		}

		symbol := strings.ToUpper(p.Symbol)
		medPrice, ok := medPriceMap[symbol]
		if !ok {
			medPrice = pricePoints{[]float64{}, []float64{}}
		}

		medPrice.prices = append(medPrice.prices, p.Price)
		medPrice.volumes = append(medPrice.volumes, p.Volume)

		medPriceMap[symbol] = medPrice
	}

	medianPricePoint := make([]p2p.AveragePricePoint, 0)
	for symbol, pricePoints := range medPriceMap {
		medianPricePoint = append(
			medianPricePoint,
			p2p.MakeAveragePricePoint(
				symbol,
				getMedian(pricePoints.prices),
				getMedian(pricePoints.volumes),
			),
		)
	}

	return medianPricePoint
}

func getMedian(buf []float64) float64 {
	if len(buf) == 0 {
		return 0
	}

	slices.Sort(buf)

	evenCount := len(buf)&1 == 0
	if evenCount {
		i := len(buf) / 2
		return (buf[i] + buf[i-1]) / 2
	} else {
		return buf[len(buf)/2]
	}
}
