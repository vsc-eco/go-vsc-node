package api

import "errors"

var ErrApiKeyNotFound = errors.New("API key not found")

type PriceQuery interface {
	Source() string
	Initialize(string) error
	Query([]string) (map[string]PricePoint, error)
}

type PricePoint struct {
	Price  float64 `json:"price"`
	Volume float64 `json:"volume"`
}
