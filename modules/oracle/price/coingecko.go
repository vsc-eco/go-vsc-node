package price

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"vsc-node/lib/utils"
	"vsc-node/modules/oracle/httputils"
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

// returns an error if the environment variable `COINGECKO_API_KEY` is not set.
// Since CoinGecko has 2 different types of API key (pro vs demo, or paid vs
// free) and their conrresponding url + header key. Assume the suppplied key
// is a pro key by default, otherwise `COINGECKO_API_DEMO` needs to be exported
// with the value '1', and attributions is required on demo keys.
func makeCoinGeckoHandler(currency string) (*coinGeckoHandler, error) {
	apiKey, ok := os.LookupEnv("COINGECKO_API_KEY")
	if !ok {
		return nil, errApiKeyNotFound
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
	url, err := httputils.MakeUrl(c.baseUrl, queries)
	if err != nil {
		return nil, err
	}

	header := map[string]string{}
	if c.demoMode {
		header["x-cg-api-key"] = c.apiKey
	} else {
		header["x-cg-pro-api-key"] = c.apiKey
	}

	req, err := httputils.MakeRequest(http.MethodGet, url, header)

	buf, err := httputils.SendRequest[[]coinGeckoPriceQueryResponse](req)
	if err != nil {
		return nil, err
	}

	return *buf, nil
}
