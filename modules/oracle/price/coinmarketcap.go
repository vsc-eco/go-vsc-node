package price

import (
	"errors"
	"os"
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
		baseUrl:  "https://sandbox-api.coinmarketcap.com/v2/cryptocurrency/quotes/latest",
		currency: "USD",
	}

	return h, nil
}

// QueryMarketPrice implements PriceQuery
func (coinmarketcaphandler *coinMarketCapHandler) QueryMarketPrice(
	watchSymbols []string,
	observePricePointChan chan<- []observePricePoint,
) {
	panic("not implemented") // TODO: Implement
}
