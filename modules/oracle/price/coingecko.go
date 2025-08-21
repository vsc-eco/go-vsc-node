package price

import (
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
)

const (
	vsCurrency = "usd"
)

type coinGeckoHandler struct {
	baseUrl    string
	apiKey     string
	httpClient *http.Client

	// since CoinGecko has 2 types of API key (demo and pro), both have
	// different base url and the header for the API key
	demoMode bool
}

func makeCoinGeckoHandler(
	apiKey string,
	demoMode bool,
) coinGeckoHandler {
	var baseUrl string
	if demoMode {
		baseUrl = "https://api.coingecko.com/api/v3"
	} else {
		baseUrl = "https://pro-api.coingecko.com/api/v3"
	}

	jar, _ := cookiejar.New(nil)
	return coinGeckoHandler{
		baseUrl:    baseUrl,
		apiKey:     apiKey,
		httpClient: &http.Client{Jar: jar},
	}
}

func (c *coinGeckoHandler) queryCoins() ([]PricePoint, error) {
	const pageLimit = 250
	var (
		page    = 1
		paths   = []string{"coins", "markets"}
		queries = map[string]string{
			"vs_currency": vsCurrency,
			"page":        fmt.Sprintf("%d", page),
			"per_page":    fmt.Sprintf("%d", pageLimit),
			"precision":   "full",
		}
	)

	out := make([]PricePoint, 0, pageLimit)

	url, err := c.makeUrl(paths, queries)
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
	pricePoints :=

		panic("TODO")
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
