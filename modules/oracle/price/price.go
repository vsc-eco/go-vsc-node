package price

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

type (
	PriceOracle struct {
		c           chan PricePoint
		avgPriceMap priceMap
	}

	PricePoint struct {
		Symbol string  `validate:"required" json:"symbol,omitempty"`
		Price  float64 `validate:"required" json:"price,omitempty"`
	}

	priceMap      map[string]avgPricePoint
	avgPricePoint struct {
		average float64
		counter uint64
	}
)

// UnmarshalJSON implements json.Unmarshaler
func (pricepoint *PricePoint) UnmarshalJSON(data []byte) error {
	type alias *PricePoint
	buf := (alias)(pricepoint)
	return json.Unmarshal(data, buf)
}

func New() PriceOracle {
	return PriceOracle{
		c:           make(chan PricePoint, 1),
		avgPriceMap: make(priceMap),
	}
}

func (p *PriceOracle) Poll(ctx context.Context, pollInterval time.Duration) {
	ticker := time.NewTicker(pollInterval)

	select {
	case <-ctx.Done():
		return

	case <-ticker.C:
		p.fetchPrices()
	}
}

func (p *PriceOracle) fetchPrices() {
	fmt.Println("TODO: implement PriceOracle.fetchPrices()")
}
