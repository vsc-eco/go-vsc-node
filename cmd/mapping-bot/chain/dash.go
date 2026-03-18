package chain

import (
	"net/http"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
)

// DASH uses the same block wire format as BTC (80-byte headers, same tx format).
// X11 PoW is not validated in the contract, so the BTC parser works.
// DASH uses P2SH instead of native SegWit on mainnet, but the mapping contract
// uses P2WSH internally so we keep the same address generator.

func dashMainNetParams() *chaincfg.Params {
	params := chaincfg.MainNetParams
	// DASH doesn't have native bech32 support in production,
	// but the VSC mapping contract uses its own address space.
	// Use BTC mainnet params as base — the contract validates internally.
	return &params
}

func dashTestNetParams() *chaincfg.Params {
	params := chaincfg.TestNet3Params
	return &params
}

func NewDASHMainnet(httpClient *http.Client) *ChainConfig {
	params := dashMainNetParams()
	return &ChainConfig{
		Name:           "dash",
		AssetSymbol:    "DASH",
		Client:         NewMempoolSpaceClient(httpClient, "https://insight.dash.org/insight-api"),
		Parser:         &BTCBlockParser{Params: params},
		AddressGen:     &BTCAddressGenerator{Params: params, BackupCSVBlocks: 17280},
		BlockInterval:  150 * time.Second, // 2.5 min blocks
		DropHeightDiff: 17280,
		ChainParams:    params,
		DefaultDbName:  "dash-mapping-bot",
	}
}

func NewDASHTestnet(httpClient *http.Client) *ChainConfig {
	params := dashTestNetParams()
	return &ChainConfig{
		Name:           "dash",
		AssetSymbol:    "DASH",
		Client:         NewMempoolSpaceClient(httpClient, "https://insight.dash.org/insight-api-testnet"),
		Parser:         &BTCBlockParser{Params: params},
		AddressGen:     &BTCAddressGenerator{Params: params, BackupCSVBlocks: 2},
		BlockInterval:  150 * time.Second,
		DropHeightDiff: 17280,
		ChainParams:    params,
		DefaultDbName:  "dash-mapping-bot-testnet",
	}
}
