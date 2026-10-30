package chain

import (
	"net/http"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
)

// BTC chain configurations

func NewBTCMainnet(httpClient *http.Client) *ChainConfig {
	return &ChainConfig{
		Name:                 "btc",
		AssetSymbol:          "BTC",
		Client:               NewMempoolSpaceClient(httpClient, "https://mempool.space/api"),
		Parser:               &BTCBlockParser{Params: &chaincfg.MainNetParams},
		AddressGen:           &BTCAddressGenerator{Params: &chaincfg.MainNetParams, BackupCSVBlocks: 4320},
		BlockInterval:        10 * time.Minute,
		SleepInterval:        time.Minute,
		DropHeightDiff:       4320, // ~30 days
		HistoricalTxLookback: 1080, // ~7.5 days
		ChainParams:          &chaincfg.MainNetParams,
		DefaultDbName:        "btc-mapping-bot",
		// contract constants.MinConfirmationDepth: 4 on mainnet
		ConfirmationsRequired: 4,
	}
}

func NewBTCTestnet4(httpClient *http.Client) *ChainConfig {
	return &ChainConfig{
		Name:                 "btc",
		AssetSymbol:          "BTC",
		Client:               NewMempoolSpaceClient(httpClient, "https://mempool.space/testnet4/api"),
		Parser:               &BTCBlockParser{Params: &chaincfg.TestNet3Params},
		AddressGen:           &BTCAddressGenerator{Params: &chaincfg.TestNet3Params, BackupCSVBlocks: 2},
		BlockInterval:        10 * time.Minute,
		SleepInterval:        10 * time.Second,
		DropHeightDiff:       4320,
		HistoricalTxLookback: 1080,
		ChainParams:          &chaincfg.TestNet3Params,
		DefaultDbName:        "btc-mapping-bot-testnet",
		// contract constants.MinConfirmationDepth: 2 on testnet3/testnet4
		ConfirmationsRequired: 2,
	}
}

func NewBTCTestnet3(httpClient *http.Client) *ChainConfig {
	return &ChainConfig{
		Name:                 "btc",
		AssetSymbol:          "BTC",
		Client:               NewMempoolSpaceClient(httpClient, "https://mempool.space/testnet/api"),
		Parser:               &BTCBlockParser{Params: &chaincfg.TestNet3Params},
		AddressGen:           &BTCAddressGenerator{Params: &chaincfg.TestNet3Params, BackupCSVBlocks: 2},
		BlockInterval:        10 * time.Minute,
		SleepInterval:        10 * time.Second,
		DropHeightDiff:       4320,
		HistoricalTxLookback: 1080,
		ChainParams:          &chaincfg.TestNet3Params,
		DefaultDbName:        "btc-mapping-bot-testnet",
		// contract constants.MinConfirmationDepth: 2 on testnet3/testnet4
		ConfirmationsRequired: 2,
	}
}

func NewBTCRegtest(httpClient *http.Client) *ChainConfig {
	return &ChainConfig{
		Name:                 "btc",
		AssetSymbol:          "BTC",
		Client:               NewMempoolSpaceClient(httpClient, "http://localhost:3000/api"),
		Parser:               &BTCBlockParser{Params: &chaincfg.RegressionNetParams},
		AddressGen:           &BTCAddressGenerator{Params: &chaincfg.RegressionNetParams, BackupCSVBlocks: 2},
		BlockInterval:        time.Second,
		SleepInterval:        time.Second,
		DropHeightDiff:       100,
		HistoricalTxLookback: 100,
		ChainParams:          &chaincfg.RegressionNetParams,
		DefaultDbName:        "btc-mapping-bot-regtest",
		// contract constants.MinConfirmationDepth: 2 on regtest
		ConfirmationsRequired: 2,
	}
}
