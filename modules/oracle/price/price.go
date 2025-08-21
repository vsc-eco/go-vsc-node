package price

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/go-playground/validator/v10"
)

var priceValidator = validator.New(validator.WithRequiredStructEnabled())

type (
	PriceOracle struct {
		c           chan PricePoint
		avgPriceMap priceMap
	}

	PricePoint struct {
		// length: range from 1-9 chars.
		// format: uppercase letters, may include numbers.
		Symbol string  `json:"symbol"        validate:"required,min=1,max=9,uppercase,alphanum"`
		Price  float64 `json:"current_price" validate:"required,gt=0.0"`
	}
)

// UnmarshalJSON implements json.Unmarshaler
func (p *PricePoint) UnmarshalJSON(data []byte) error {
	type alias *PricePoint
	buf := (alias)(p)

	if err := json.Unmarshal(data, buf); err != nil {
		return err
	}

	return priceValidator.Struct(p)
}

func New() PriceOracle {
	return PriceOracle{
		c:           make(chan PricePoint, 1),
		avgPriceMap: priceMap{},
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
