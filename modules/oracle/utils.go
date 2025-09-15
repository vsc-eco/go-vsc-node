package oracle

import (
	"errors"
	"slices"
	"sync"
	"time"
	"vsc-node/modules/oracle/p2p"

	"github.com/libp2p/go-libp2p/core/peer"
)

const (
	listenDuration = time.Second * 10
	hourInSecond   = 3600
)

type pricePoints struct {
	price  float64
	volume float64
	peerID peer.ID

	// unix UTC timestamp
	collectedAt int64
}

type medianPricePointMap = map[string]pricePoints

func makeMedianPrices(
	avgPricePoints map[string]p2p.AveragePricePoint,
) map[string]p2p.AveragePricePoint {
	/*
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
					getMedian(pricePoints.prices),
					getMedian(pricePoints.volumes),
				),
			)
		}

		return medianPricePoint
	*/
	return nil
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

func parseMsg[T any](data any) (*T, error) {
	v, ok := data.(T)
	if !ok {
		return nil, errors.New("invalid type")
	}
	return &v, nil
}

type threadSafeMap[K comparable, V any] struct {
	buf map[K]V
	mtx *sync.Mutex
}

func makeThreadSafeMap[K comparable, V any]() *threadSafeMap[K, V] {
	return &threadSafeMap[K, V]{
		buf: make(map[K]V),
		mtx: new(sync.Mutex),
	}
}

// argument is nil if the key does not exist in internal map
type updateFunc[K comparable, V any] func(map[K]V)

func (t *threadSafeMap[K, V]) Update(updateFunc updateFunc[K, V]) {
	t.mtx.Lock()
	defer t.mtx.Unlock()
	updateFunc(t.buf)
}
