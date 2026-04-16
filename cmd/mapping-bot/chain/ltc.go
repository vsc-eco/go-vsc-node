package chain

import (
	"net/http"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
)

// LTC uses the same block wire format as BTC, but with different network params
// and a different block explorer API. Scrypt PoW is not validated in the contract
// so the BTC parser/address generator work as-is with LTC chaincfg params.

// ltcMainNetParams returns Litecoin mainnet parameters.
// These are BTC TestNet3Params modified for LTC's address prefixes.
func ltcMainNetParams() *chaincfg.Params {
	params := chaincfg.MainNetParams
	params.Bech32HRPSegwit = "ltc"
	return &params
}

func ltcTestNetParams() *chaincfg.Params {
	params := chaincfg.TestNet3Params
	params.Bech32HRPSegwit = "tltc"
	return &params
}

func NewLTCMainnet(httpClient *http.Client) *ChainConfig {
	params := ltcMainNetParams()
	return &ChainConfig{
		Name:                 "ltc",
		AssetSymbol:          "LTC",
		Client:               NewMempoolSpaceClient(httpClient, "https://litecoinspace.org/api"),
		Parser:               &BTCBlockParser{Params: params},
		AddressGen:           &BTCAddressGenerator{Params: params, BackupCSVBlocks: 17280},
		BlockInterval:        150 * time.Second, // 2.5 min blocks
		SleepInterval:        time.Minute,
		DropHeightDiff:       17280, // ~30 days at 2.5 min
		HistoricalTxLookback: 4032,  // ~7 days at 2.5 min
		ChainParams:          params,
		DefaultDbName:        "ltc-mapping-bot",
	}
}

func NewLTCTestnet(httpClient *http.Client) *ChainConfig {
	params := ltcTestNetParams()
	return &ChainConfig{
		Name:                 "ltc",
		AssetSymbol:          "LTC",
		Client:               NewMempoolSpaceClient(httpClient, "https://litecoinspace.org/testnet/api"),
		Parser:               &BTCBlockParser{Params: params},
		AddressGen:           &BTCAddressGenerator{Params: params, BackupCSVBlocks: 2},
		BlockInterval:        150 * time.Second,
		SleepInterval:        10 * time.Second,
		DropHeightDiff:       17280,
		HistoricalTxLookback: 4032,
		ChainParams:          params,
		DefaultDbName:        "ltc-mapping-bot-testnet",
	}
}
