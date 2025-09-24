package price

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"vsc-node/lib/utils"
	"vsc-node/modules/oracle/httputils"
	"vsc-node/modules/oracle/p2p"
)

type coinMarketCapHandler struct {
	baseUrl  string
	apiKey   string
	currency string
}

var _ priceQuery = &coinMarketCapHandler{}

// initialize implements priceQuery
// returns an error if the environment variable `COINMARKETCAP_API_KEY` is not
// set
func (c *coinMarketCapHandler) initialize(currency string) error {
	apiKey, ok := os.LookupEnv("COINMARKETCAP_API_KEY")
	if !ok {
		return errApiKeyNotFound
	}

	c = &coinMarketCapHandler{
		apiKey:   apiKey,
		baseUrl:  "https://pro-api.coinmarketcap.com/v1/cryptocurrency/quotes/latest",
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

// queryMarketPrice implements priceQuery
func (c *coinMarketCapHandler) queryMarketPrice(
	watchSymbols []string) (map[string]p2p.ObservePricePoint, error) {
	symbols := make([]string, len(watchSymbols))
	copy(symbols, watchSymbols)
	symbols = utils.Map(watchSymbols, strings.ToUpper)

	marketPrices, err := c.fetchPrices(symbols)
	if err != nil {
		log.Println("[coinmarketcap] failed to query market data:", err)
	}

	observePricePoints := make(map[string]p2p.ObservePricePoint)
	for symbol, marketData := range marketPrices.Data {
		o, err := marketData.makeObservePricePoint(c.currency)
		if err != nil {
			return nil, fmt.Errorf(
				"failed to parse symbol [%s]: %e",
				symbol,
				err,
			)
		}
		observePricePoints[symbol] = *o
	}

	return observePricePoints, nil
}

func (c *coinMarketCapHandler) fetchPrices(
	symbols []string,
) (*coinMarketCapApiResponse, error) {
	queryParams := map[string]string{
		"symbol":  strings.Join(symbols, ","),
		"convert": c.currency,
	}

	url, err := httputils.MakeUrl(c.baseUrl, queryParams)
	if err != nil {
		return nil, fmt.Errorf("failed to build url: %e", err)
	}

	header := map[string]string{"X-CMC_PRO_API_KEY": c.apiKey}

	req, err := httputils.MakeRequest(http.MethodGet, url, header)
	if err != nil {
		return nil, fmt.Errorf("failed to make request: %e", err)
	}

	return httputils.SendRequest[coinMarketCapApiResponse](req)
}

func (c *coinMarketCapData) makeObservePricePoint(
	currency string,
) (*p2p.ObservePricePoint, error) {
	quote, ok := c.Quote[currency]
	if !ok {
		return nil, fmt.Errorf("currency not converted: %s", currency)
	}

	out := &p2p.ObservePricePoint{
		Symbol: c.Symbol,
		Price:  quote.Price,
		Volume: quote.Volume,
	}

	return out, nil
}
