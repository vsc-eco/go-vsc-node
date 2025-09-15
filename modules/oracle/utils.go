package oracle

import (
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
)

const (
	listenDuration = time.Second * 10
	hourInSecond   = 3600
)

type pricePoint struct {
	price  float64
	volume float64
	peerID peer.ID

	// unix UTC timestamp
	collectedAt time.Time
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

// returns a copy of the internal map
func (t threadSafeMap[K, V]) GetMap() map[K]V {
	t.mtx.Lock()
	defer t.mtx.Unlock()

	bufCpy := make(map[K]V)
	for k, v := range t.buf {
		bufCpy[k] = v
	}

	return bufCpy
}
