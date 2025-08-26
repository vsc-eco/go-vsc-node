package price

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"vsc-node/lib/utils"
)

const (
	pageLimit = 250
)

type coinGeckoHandler struct {
	baseUrl  string
	apiKey   string
	currency string

	// since CoinGecko has 2 types of API key (demo and pro), both have
	// different base url and the header for the API key
	demoMode bool
}

type coinGeckoPriceQueryResponse struct {
	Symbol       string  `json:"symbol,omitempty"`
	CurrentPrice float64 `json:"current_price,omitempty"`
	TotalVolume  uint64  `json:"total_volume,omitempty"`
}

func makeCoinGeckoHandler(currency string) (*coinGeckoHandler, error) {
	apiKey, ok := os.LookupEnv("COINGECKO_API_KEY")
	if !ok {
		return nil, errApiKeyNotFound
	}

	var (
		baseUrl  string
		demoMode = os.Getenv("COINGECKO_API_PRO") != "1"
	)

	if demoMode {
		// CoinGecko requires attribution on free tier
		// https://brand.coingecko.com/resources/attribution-guide
		fmt.Println("Price data by [CoinGecko](https://www.coingecko.com)")
		baseUrl = "https://api.coingecko.com/api/v3/coins/markets"
	} else {
		baseUrl = "https://pro-api.coingecko.com/api/v3/coins/markets"
	}

	h := &coinGeckoHandler{
		baseUrl:  baseUrl,
		apiKey:   apiKey,
		currency: strings.ToLower(currency),
		demoMode: demoMode,
	}

	return h, nil
}

func (c *coinGeckoHandler) QueryMarketPrice(
	symbols []string,
	pricePointChan chan<- []observePricePoint,
) {
	symLowerCase := make([]string, len(symbols))
	copy(symLowerCase, symbols)

	symLowerCase = utils.Map(symLowerCase, strings.ToLower)

	queries := map[string]string{
		"vs_currency": c.currency,
		"per_page":    fmt.Sprintf("%d", pageLimit),
		"precision":   "full",
		"symbols":     strings.Join(symLowerCase, ","),
	}

	fetchedPrice, err := c.fetchPrices(queries)
	if err != nil {
		log.Println("failed to fetch market price", err)
		return
	}

	pricePointChan <- utils.Map(fetchedPrice, mapCgResponse)
}

func mapCgResponse(p coinGeckoPriceQueryResponse) observePricePoint {
	return observePricePoint{
		symbol: strings.ToUpper(p.Symbol),
		price:  p.CurrentPrice,
		volume: float64(p.TotalVolume),
	}
}

// market values queried from
// https://docs.coingecko.com/reference/coins-markets
func (c *coinGeckoHandler) fetchPrices(
	queries map[string]string,
) ([]coinGeckoPriceQueryResponse, error) {
	url, err := makeUrl(c.baseUrl, queries)
	if err != nil {
		return nil, err
	}

	header := map[string]string{}
	if c.demoMode {
		header["x-cg-api-key"] = c.apiKey
	} else {
		header["x-cg-pro-api-key"] = c.apiKey
	}

	res, err := makeRequest(http.MethodGet, url, header)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %s", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		buf := make(map[string]any)
		if err := json.NewDecoder(res.Body).Decode(&buf); err != nil {
			return nil, fmt.Errorf("failed to decode error messages: %s", err)
		}

		return nil, fmt.Errorf(
			"request failed, http status: %s, error: %s",
			res.Status, buf,
		)
	}

	buf := make([]coinGeckoPriceQueryResponse, 0, pageLimit)
	if err := json.NewDecoder(res.Body).Decode(&buf); err != nil {
		return nil, fmt.Errorf("failed to decode response: %s", err)
	}

	return buf, nil
}
