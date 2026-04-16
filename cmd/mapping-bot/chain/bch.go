package chain

import (
	"net/http"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
)

// BCH uses the exact same block wire format as BTC (SHA256 PoW, same tx format).
// 10-minute block time, same P2WSH address format. CashAddr encoding is handled
// externally — the contract uses standard btcsuite address encoding.

func bchMainNetParams() *chaincfg.Params {
	params := chaincfg.MainNetParams
	// BCH uses "bitcoincash:" prefix for CashAddr but the contract
	// uses standard bech32/P2WSH internally.
	return &params
}

func bchTestNetParams() *chaincfg.Params {
	params := chaincfg.TestNet3Params
	return &params
}

func NewBCHMainnet(httpClient *http.Client) *ChainConfig {
	params := bchMainNetParams()
	return &ChainConfig{
		Name:                 "bch",
		AssetSymbol:          "BCH",
		Client:               NewMempoolSpaceClient(httpClient, "https://blockchair.com/bitcoin-cash/api"),
		Parser:               &BTCBlockParser{Params: params},
		AddressGen:           &BTCAddressGenerator{Params: params, BackupCSVBlocks: 4320},
		BlockInterval:        10 * time.Minute, // same as BTC
		SleepInterval:        time.Minute,
		DropHeightDiff:       4320, // ~30 days
		HistoricalTxLookback: 1080, // ~7 days at 10 min
		ChainParams:          params,
		DefaultDbName:        "bch-mapping-bot",
	}
}

func NewBCHTestnet(httpClient *http.Client) *ChainConfig {
	params := bchTestNetParams()
	return &ChainConfig{
		Name:                 "bch",
		AssetSymbol:          "BCH",
		Client:               NewMempoolSpaceClient(httpClient, "https://blockchair.com/bitcoin-cash/testnet/api"),
		Parser:               &BTCBlockParser{Params: params},
		AddressGen:           &BTCAddressGenerator{Params: params, BackupCSVBlocks: 2},
		BlockInterval:        10 * time.Minute,
		SleepInterval:        10 * time.Second,
		DropHeightDiff:       4320,
		HistoricalTxLookback: 1080,
		ChainParams:          params,
		DefaultDbName:        "bch-mapping-bot-testnet",
	}
}
