package api

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"vsc-node/lib/utils"
)

const coinMarketCapQuoteUrl = "https://pro-api.coinmarketcap.com/v1/cryptocurrency/quotes/latest"

type CoinMarketCap struct {
	apiKey   string
	currency string
}

var _ PriceQuery = &CoinMarketCap{}

// Source implements PriceQuery
func (c *CoinMarketCap) Source() string {
	return "CoinMarketCap"
}

// Initialize implements PriceQuery
// returns an error if the environment variable `COINMARKETCAP_API_KEY` is not set
func (c *CoinMarketCap) Initialize(currency string) error {
	apiKey, ok := os.LookupEnv("COINMARKETCAP_API_KEY")
	if !ok {
		return ErrApiKeyNotFound
	}

	*c = CoinMarketCap{
		apiKey:   apiKey,
		currency: strings.ToUpper(currency),
	}

	return nil
}

type coinMarketCapApiResponse struct {
	Data map[string]coinMarketCapData `json:"data"`
}

type coinMarketCapData struct {
	Name   string                        `json:"name,omitempty"`
	Symbol string                        `json:"symbol,omitempty"`
	Quote  map[string]coinMarketCapQuote `json:"quote,omitempty"`
}

type coinMarketCapQuote struct {
	Price  float64 `json:"price,omitempty"`
	Volume float64 `json:"volume_24h,omitempty"`
}

// Query implements PriceQuery
func (c *CoinMarketCap) Query(
	watchSymbols []string,
) (map[string]PricePoint, error) {
	symbols := utils.Map(watchSymbols, strings.ToUpper)

	marketPrices, err := c.fetchPrices(symbols)
	if err != nil {
		return nil, fmt.Errorf("failed to query market data: %w", err)
	}

	observePricePoints := make(map[string]PricePoint)
	for symbol, marketData := range marketPrices.Data {
		quote, ok := marketData.Quote[c.currency]
		if !ok {
			return nil, fmt.Errorf("currency not found: %s", c.currency)
		}

		observePricePoints[symbol] = PricePoint{
			Price:  quote.Price,
			Volume: quote.Volume,
		}
	}

	return observePricePoints, nil
}

func (c *CoinMarketCap) fetchPrices(symbols []string) (*coinMarketCapApiResponse, error) {
	queryParams := map[string]string{
		"symbol":  strings.Join(symbols, ","),
		"convert": c.currency,
	}

	url, err := makeUrl(coinMarketCapQuoteUrl, queryParams)
	if err != nil {
		return nil, fmt.Errorf("failed to build url: %w", err)
	}

	header := map[string]string{"X-CMC_PRO_API_KEY": c.apiKey}

	req, err := makeRequest(http.MethodGet, url, header)
	if err != nil {
		return nil, fmt.Errorf("failed to make request: %e", err)
	}

	return httpRequest[coinMarketCapApiResponse](req)
}
