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
	Data   map[string]coinMarketCapData `json:"data"`
	Status coinMarketCapStatus          `json:"status"`
}

type coinMarketCapData struct {
	Name   string                        `json:"name,omitempty"`
	Symbol string                        `json:"symbol,omitempty"`
	Quote  map[string]coinMarketCapQuote `json:"quote,omitempty"`
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

type coinMarketCapQuote struct {
	Price  float64 `json:"price,omitempty"`
	Volume float64 `json:"volume_24h,omitempty"`
}

type coinMarketCapStatus struct {
	ErrorCode    int    `json:"error_code,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
}

// QueryMarketPrice implements PriceQuery
func (c *coinMarketCapHandler) QueryMarketPrice(
	watchSymbols []string,
	observePricePointChan chan<- []observePricePoint,
) {
	symbols := make([]string, len(watchSymbols))
	copy(symbols, watchSymbols)
	symbols = utils.Map(watchSymbols, strings.ToUpper)

	queryParams := map[string]string{
		"symbol":  strings.Join(symbols, ","),
		"convert": c.currency,
	}
	url, err := makeUrl(c.baseUrl, queryParams)
	if err != nil {
		log.Println("[coinmarketcap] invalid url:", err)
		return
	}

	header := map[string]string{"X-CMC_PRO_API_KEY": c.apiKey}
	resp, err := makeRequest(http.MethodGet, url, header)
	if err != nil {
		log.Println("invalid failed to build request:", err)
		return
	}

	defer resp.Body.Close()

	buf := coinMarketCapApiResponse{}
	if err := json.NewDecoder(resp.Body).Decode(&buf); err != nil {
		log.Println("invalid json body response:", err)
		return
	}

	observePricePoints := make([]observePricePoint, 0, len(watchSymbols))
	for symbol, marketData := range buf.Data {
		o, err := marketData.makeObservePricePoint(c.currency)
		if err != nil {
			log.Printf("failed to parse symbol [%s]: %e", symbol, err)
			continue
		}
		observePricePoints = append(observePricePoints, *o)
	}

	observePricePointChan <- observePricePoints
}
