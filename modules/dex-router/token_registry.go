package dex_router

import (
	"fmt"
	"vsc-node/lib/logger"
	"vsc-node/modules/db/vsc/dex"
)

type TokenRegistry struct {
	dexDb dex.DexDb
	log   logger.Logger
}

func NewTokenRegistry(dexDb dex.DexDb, log logger.Logger) *TokenRegistry {
	registry := &TokenRegistry{
		dexDb: dexDb,
		log:   log,
	}

	// Initialize with native assets
	registry.initializeNativeAssets()

	return registry
}

func (tr *TokenRegistry) initializeNativeAssets() {
	nativeAssets := []dex.TokenMetadata{
		{
			Symbol:      "HBD",
			Decimals:    3,
			ContractId:  nil, // native asset
			Description: "Hive Backed Dollar",
		},
		{
			Symbol:      "HIVE",
			Decimals:    3,
			ContractId:  nil, // native asset
			Description: "Hive cryptocurrency",
		},
		{
			Symbol:      "HBD_SAVINGS",
			Decimals:    3,
			ContractId:  nil, // native asset
			Description: "Hive Backed Dollar Savings",
		},
	}

	for _, asset := range nativeAssets {
		existing, err := tr.dexDb.GetTokenMetadata(asset.Symbol)
		if err != nil {
			// Token doesn't exist, create it
			tr.dexDb.SetTokenMetadata(&asset)
			tr.log.Debug("Initialized native asset", "symbol", asset.Symbol)
		} else {
			// Update if needed
			if existing.Decimals != asset.Decimals || existing.Description != asset.Description {
				asset.CreatedAt = existing.CreatedAt
				tr.dexDb.SetTokenMetadata(&asset)
				tr.log.Debug("Updated native asset", "symbol", asset.Symbol)
			}
		}
	}
}

// GetTokenDecimals returns the decimal precision for a given asset
func (tr *TokenRegistry) GetTokenDecimals(asset string) (uint8, error) {
	token, err := tr.dexDb.GetTokenMetadata(asset)
	if err != nil {
		return 0, fmt.Errorf("token not found: %s", asset)
	}
	return token.Decimals, nil
}

// GetTokenContract returns the contract ID for a token, nil for native assets
func (tr *TokenRegistry) GetTokenContract(asset string) (*string, error) {
	token, err := tr.dexDb.GetTokenMetadata(asset)
	if err != nil {
		return nil, fmt.Errorf("token not found: %s", asset)
	}
	return token.ContractId, nil
}

// IsNativeAsset checks if an asset is a native Hive asset
func (tr *TokenRegistry) IsNativeAsset(asset string) bool {
	contractId, err := tr.GetTokenContract(asset)
	if err != nil {
		return false
	}
	return contractId == nil
}

// RegisterToken registers a new token in the registry
func (tr *TokenRegistry) RegisterToken(symbol string, decimals uint8, contractId *string, description string) error {
	token := &dex.TokenMetadata{
		Symbol:      symbol,
		Decimals:    decimals,
		ContractId:  contractId,
		Description: description,
	}

	return tr.dexDb.SetTokenMetadata(token)
}

// GetAllTokens returns all registered tokens
func (tr *TokenRegistry) GetAllTokens() ([]dex.TokenMetadata, error) {
	return tr.dexDb.GetAllTokens()
}

// GetTokenInfo returns full token metadata
func (tr *TokenRegistry) GetTokenInfo(asset string) (*dex.TokenMetadata, error) {
	return tr.dexDb.GetTokenMetadata(asset)
}
