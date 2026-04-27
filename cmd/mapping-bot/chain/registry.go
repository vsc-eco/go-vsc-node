package chain

import (
	"fmt"
	"net/http"
)

// Resolve returns the ChainConfig for the given chain name and network.
// Chain names: "btc", "ltc", "dash"
// Networks: "mainnet", "testnet", "testnet3", "testnet4", "regtest"
//
// To add a new chain:
// 1. Create a new file (e.g., chain/sol.go) with NewSOLMainnet/NewSOLTestnet
// 2. Implement BlockchainClient, BlockParser, AddressGenerator for the chain
// 3. Add entries to the switch below
func Resolve(chainName, network string, httpClient *http.Client) (*ChainConfig, error) {
	switch chainName {
	case "btc":
		switch network {
		case "mainnet":
			return NewBTCMainnet(httpClient), nil
		case "testnet4":
			return NewBTCTestnet4(httpClient), nil
		case "testnet", "testnet3":
			return NewBTCTestnet3(httpClient), nil
		case "regtest":
			return NewBTCRegtest(httpClient), nil
		}
	case "ltc":
		switch network {
		case "mainnet":
			return NewLTCMainnet(httpClient), nil
		case "testnet":
			return NewLTCTestnet(httpClient), nil
		}
	case "dash":
		switch network {
		case "mainnet":
			return NewDASHMainnet(httpClient), nil
		case "testnet":
			return NewDASHTestnet(httpClient), nil
		case "regtest":
			return NewDASHRegtest(httpClient), nil
		}
	case "doge":
		switch network {
		case "mainnet":
			return NewDOGEMainnet(httpClient), nil
		case "testnet":
			return NewDOGETestnet(httpClient), nil
		}
	case "bch":
		switch network {
		case "mainnet":
			return NewBCHMainnet(httpClient), nil
		case "testnet":
			return NewBCHTestnet(httpClient), nil
		}
	}
	return nil, fmt.Errorf("unsupported chain/network: %s/%s", chainName, network)
}

// SupportedChains returns a list of supported chain names for help text.
func SupportedChains() []string {
	return []string{"btc", "ltc", "dash", "doge", "bch"}
}
