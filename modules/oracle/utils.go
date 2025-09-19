package oracle

import (
	"encoding/json"
	"math"
	"slices"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
)

const (
	hourInSecond = 3600

	float64Epsilon = 1e-9
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

func parseRawJson[T any](data json.RawMessage) (*T, error) {
	v := new(T)
	if err := json.Unmarshal(data, v); err != nil {
		return nil, err
	}
	return v, nil
}

func float64Eq(a, b float64) bool {
	return math.Abs(a-b) < float64Epsilon
}
