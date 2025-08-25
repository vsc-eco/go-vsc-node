package price

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"vsc-node/lib/utils"
)

const (
	pageLimit = 250
)

type coinGeckoHandler struct {
	baseUrl    string
	apiKey     string
	vsCurrency string
	httpClient *http.Client

	// since CoinGecko has 2 types of API key (demo and pro), both have
	// different base url and the header for the API key
	demoMode bool
}

type coinGeckoPriceQueryResponse struct {
	Symbol       string  `json:"symbol,omitempty"`
	CurrentPrice float64 `json:"current_price,omitempty"`
	TotalVolume  uint64  `json:"total_volume,omitempty"`
}

func makeCoinGeckoHandler(
	apiKey string,
	demoMode bool,
	vsCurrency string, // likely have to load from user's config
) coinGeckoHandler {
	var baseUrl string
	if demoMode {
		// CoinGecko requires attribution on free tier
		// https://brand.coingecko.com/resources/attribution-guide
		fmt.Println("Price data by [CoinGecko](https://www.coingecko.com)")
		baseUrl = "https://api.coingecko.com/api/v3"
	} else {
		baseUrl = "https://pro-api.coingecko.com/api/v3"
	}

	jar, _ := cookiejar.New(nil)

	return coinGeckoHandler{
		baseUrl:    baseUrl,
		apiKey:     apiKey,
		vsCurrency: vsCurrency,
		demoMode:   demoMode,
		httpClient: &http.Client{Jar: jar},
	}
}

func (c *coinGeckoHandler) QueryMarketPrice(
	symbols []string,
	pricePointChan chan<- []observePricePoint,
) {
	symLowerCase := make([]string, len(symbols))
	copy(symLowerCase, symbols)

	symLowerCase = utils.Map(symLowerCase, strings.ToLower)

	queries := map[string]string{
		"vs_currency": c.vsCurrency,
		"per_page":    fmt.Sprintf("%d", pageLimit),
		"precision":   "full",
		"symbols":     strings.Join(symLowerCase, ","),
	}
	paths := [...]string{"coins", "markets"}

	fetchedPrice, err := c.fetchPrices(paths[:], queries)
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
	urlPaths []string,
	queries map[string]string,
) ([]coinGeckoPriceQueryResponse, error) {
	url, err := c.makeUrl(urlPaths, queries)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodGet, url.String(), nil)
	if err != nil {
		return nil, err
	}

	if c.demoMode {
		req.Header.Add("x-cg-api-key", c.apiKey)
	} else {
		req.Header.Add("x-cg-pro-api-key", c.apiKey)
	}

	res, err := c.httpClient.Do(req)
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

func (c *coinGeckoHandler) makeUrl(
	pathParams []string,
	queryParams map[string]string,
) (*url.URL, error) {
	url, err := url.Parse(c.baseUrl)
	if err != nil {
		return nil, err
	}

	if pathParams != nil {
		url = url.JoinPath(pathParams...)
	}

	if queryParams != nil {
		q := url.Query()
		for key, val := range queryParams {
			q.Add(key, val)
		}

		url.RawQuery = q.Encode()
	}

	return url, nil
}
