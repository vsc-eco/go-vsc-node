package chain

import (
	"net/http"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
)

// DOGE uses the same block wire format as BTC/LTC (Scrypt PoW, not validated in contract).
// 1-minute block time, same P2WSH address format.

func dogeMainNetParams() *chaincfg.Params {
	params := chaincfg.MainNetParams
	// DOGE doesn't have native bech32 — the mapping contract manages its own address space.
	return &params
}

func dogeTestNetParams() *chaincfg.Params {
	params := chaincfg.TestNet3Params
	return &params
}

func NewDOGEMainnet(httpClient *http.Client) *ChainConfig {
	params := dogeMainNetParams()
	return &ChainConfig{
		Name:           "doge",
		AssetSymbol:    "DOGE",
		Client:         NewMempoolSpaceClient(httpClient, "https://blockchair.com/dogecoin/api"),
		Parser:         &BTCBlockParser{Params: params},
		AddressGen:     &BTCAddressGenerator{Params: params, BackupCSVBlocks: 43200},
		BlockInterval:  time.Minute, // 1 min blocks
		SleepInterval:  time.Minute,
		DropHeightDiff: 43200, // ~30 days at 1 min
		ChainParams:    params,
		DefaultDbName:  "doge-mapping-bot",
	}
}

func NewDOGETestnet(httpClient *http.Client) *ChainConfig {
	params := dogeTestNetParams()
	return &ChainConfig{
		Name:           "doge",
		AssetSymbol:    "DOGE",
		Client:         NewMempoolSpaceClient(httpClient, "https://blockchair.com/dogecoin/testnet/api"),
		Parser:         &BTCBlockParser{Params: params},
		AddressGen:     &BTCAddressGenerator{Params: params, BackupCSVBlocks: 2},
		BlockInterval:  time.Minute,
		SleepInterval:  10 * time.Second,
		DropHeightDiff: 43200,
		ChainParams:    params,
		DefaultDbName:  "doge-mapping-bot-testnet",
	}
}
