package price

import (
	"encoding/json"
	"fmt"
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
	// TODO: initialize this thing
	fmt.Println("alskdjfkljdsf")
	return PriceOracle{}
}
