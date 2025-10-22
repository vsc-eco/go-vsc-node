package api

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"vsc-node/lib/utils"
)

const (
	pageLimit = 250
)

type CoinGecko struct {
	baseUrl  string
	apiKey   string
	currency string

	// since CoinGecko has 2 types of API key (demo and pro), both have
	// different base url and the header for the API key
	demoMode bool
}

// var _ PriceQuery = &CoinGecko{}

// Source implements PriceQuery
func (c *CoinGecko) Source() string {
	return "CoinGecko"
}

// Initialize implements PriceQuery
// returns an error if the environment variable `COINGECKO_API_KEY` is not set.
// Since CoinGecko has 2 different types of API key (pro vs demo, or paid vs
// free) and their conrresponding url + header key. Assume the suppplied key
// is a pro key by default, otherwise `COINGECKO_API_DEMO` needs to be exported
// with the value '1', and attributions is required on demo keys.
func (c *CoinGecko) Initialize(currency string) error {
	apiKey, ok := os.LookupEnv("COINGECKO_API_KEY")
	if !ok {
		return ErrApiKeyNotFound
	}

	var (
		baseUrl  string
		demoMode = os.Getenv("COINGECKO_API_DEMO") == "1"
	)

	if demoMode {
		// CoinGecko requires attribution on free tier
		// https://brand.coingecko.com/resources/attribution-guide
		fmt.Println("Price data by [CoinGecko](https://www.coingecko.com)")
		baseUrl = "https://api.coingecko.com/api/v3/coins/markets"
	} else {
		baseUrl = "https://pro-api.coingecko.com/api/v3/coins/markets"
	}

	*c = CoinGecko{
		baseUrl:  baseUrl,
		apiKey:   apiKey,
		currency: strings.ToLower(currency),
		demoMode: demoMode,
	}

	return nil
}

// Query implements PriceQuery
func (c *CoinGecko) Query(symbols []string) (map[string]PricePoint, error) {
	symbols = utils.Map(symbols, strings.ToLower)

	queries := map[string]string{
		"vs_currency": c.currency,
		"per_page":    fmt.Sprintf("%d", pageLimit),
		"precision":   "full",
		"symbols":     strings.Join(symbols, ","),
	}

	fetchedPrice, err := c.fetchPrices(queries)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch market price: %w", err)
	}

	buf := make(map[string]PricePoint)
	for _, p := range fetchedPrice {
		s := strings.ToLower(p.Symbol)
		buf[s] = PricePoint{
			Price:  p.CurrentPrice,
			Volume: float64(p.TotalVolume),
		}
	}

	return buf, nil
}

type coinGeckoPriceQueryResponse struct {
	Symbol       string  `json:"symbol,omitempty"`
	CurrentPrice float64 `json:"current_price,omitempty"`
	TotalVolume  uint64  `json:"total_volume,omitempty"`
}

// market values queried from
// https://docs.coingecko.com/reference/coins-markets
func (c *CoinGecko) fetchPrices(
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

	req, err := makeRequest(http.MethodGet, url, header)

	buf, err := httpRequest[[]coinGeckoPriceQueryResponse](req)
	if err != nil {
		return nil, err
	}

	return *buf, nil
}
