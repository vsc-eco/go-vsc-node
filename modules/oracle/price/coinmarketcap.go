package price

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"vsc-node/lib/utils"
)

var (
	errApiKeyNotFound = errors.New("API key not exported")
)

type coinMarketCapHandler struct {
	baseUrl  string
	apiKey   string
	currency string
}

func makeCoinMarketCapHandler() (*coinMarketCapHandler, error) {
	apiKey, ok := os.LookupEnv("COINMARKETCAP_API_KEY")
	if !ok {
		return nil, errApiKeyNotFound
	}

	h := &coinMarketCapHandler{
		apiKey:   apiKey,
		baseUrl:  "https://pro-api.coinmarketcap.com/v1/cryptocurrency/quotes/latest",
		currency: "USD",
	}

	return h, nil
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

// QueryMarketPrice implements PriceQuery
func (c *coinMarketCapHandler) QueryMarketPrice(
	watchSymbols []string,
	observePricePointChan chan<- []observePricePoint,
) {
	symbols := make([]string, len(watchSymbols))
	copy(symbols, watchSymbols)
	symbols = utils.Map(watchSymbols, strings.ToUpper)

	marketPrices, err := c.fetchPrices(symbols)
	if err != nil {
		log.Println("[coinmarketcap] failed to query market data:", err)
	}

	observePricePoints := make([]observePricePoint, 0, len(watchSymbols))
	for symbol, marketData := range marketPrices.Data {
		o, err := marketData.makeObservePricePoint(c.currency)
		if err != nil {
			log.Printf("failed to parse symbol [%s]: %e", symbol, err)
			continue
		}
		observePricePoints = append(observePricePoints, *o)
	}

	observePricePointChan <- observePricePoints
}

func (c *coinMarketCapHandler) fetchPrices(
	symbols []string,
) (*coinMarketCapApiResponse, error) {
	queryParams := map[string]string{
		"symbol":  strings.Join(symbols, ","),
		"convert": c.currency,
	}

	url, err := makeUrl(c.baseUrl, queryParams)
	if err != nil {
		return nil, fmt.Errorf("failed to build url: %e", err)
	}

	header := map[string]string{"X-CMC_PRO_API_KEY": c.apiKey}

	resp, err := makeRequest(http.MethodGet, url, header)
	if err != nil {
		return nil, fmt.Errorf("failed to make request: %e", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		buf := make(map[string]any)
		if err := json.NewDecoder(resp.Body).Decode(&buf); err != nil {
			return nil, fmt.Errorf("failed to decode error messages: %s", err)
		}

		return nil, fmt.Errorf(
			"request failed, http status: %s, error: %s",
			resp.Status, buf,
		)
	}

	buf := &coinMarketCapApiResponse{}
	if err := json.NewDecoder(resp.Body).Decode(buf); err != nil {
		return nil, fmt.Errorf("failed to deserialize json data: %e", err)
	}

	return buf, nil
}

func (c *coinMarketCapData) makeObservePricePoint(
	currency string,
) (*observePricePoint, error) {
	quote, ok := c.Quote[currency]
	if !ok {
		return nil, fmt.Errorf("currency not converted: %s", currency)
	}

	out := &observePricePoint{
		symbol: c.Symbol,
		price:  quote.Price,
		volume: quote.Volume,
	}

	return out, nil
}
