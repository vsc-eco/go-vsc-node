package dex_router

import (
	"fmt"
	"time"
	"vsc-node/lib/logger"
	"vsc-node/modules/db/vsc/dex"
)

type Router struct {
	dexDb           dex.DexDb
	tokenRegistry   *TokenRegistry
	bondingCurveMgr *BondingCurveManager
	log             logger.Logger
}

func NewRouter(dexDb dex.DexDb, log logger.Logger) *Router {
	tokenRegistry := NewTokenRegistry(dexDb, log)
	bondingCurveMgr := NewBondingCurveManager(dexDb, log)

	return &Router{
		dexDb:           dexDb,
		tokenRegistry:   tokenRegistry,
		bondingCurveMgr: bondingCurveMgr,
		log:             log,
	}
}

// GeneratePairAccount generates a DEX pair account for a specific chain
func (r *Router) GeneratePairAccount(asset0, asset1, chain string) string {
	// Ensure assets are in consistent order (alphabetical)
	if asset0 > asset1 {
		asset0, asset1 = asset1, asset0
	}
	return fmt.Sprintf("%s:dex-pair-%s-%s", chain, asset0, asset1)
}

// RegisterPool registers a new liquidity pool with optional cross-chain support
func (r *Router) RegisterPool(contractId, asset0, asset1, creator string, creationHeight uint64, targetChains []string) error {
	// Validate that both assets exist in registry
	_, err := r.tokenRegistry.GetTokenInfo(asset0)
	if err != nil {
		return fmt.Errorf("asset0 not found in registry: %s", asset0)
	}

	_, err = r.tokenRegistry.GetTokenInfo(asset1)
	if err != nil {
		return fmt.Errorf("asset1 not found in registry: %s", asset1)
	}

	// Get DEX parameters for bonding curve target
	params, err := r.dexDb.GetDexParams()
	if err != nil {
		return fmt.Errorf("failed to get DEX parameters: %w", err)
	}

	// Generate pair accounts for each target chain
	pairAccounts := make(map[string]string)
	for _, chain := range targetChains {
		pairAccounts[chain] = r.GeneratePairAccount(asset0, asset1, chain)
	}

	pool := &dex.PoolInfo{
		ContractId:     contractId,
		Asset0:         asset0,
		Asset1:         asset1,
		Creator:        creator,
		CreationHeight: creationHeight,
		LockedLPAmount: 0, // Creator's initial LP tokens (set after first liquidity provision)
		BondingMetric:  0,
		BondingTarget:  params.BondingCurveThreshold,
		Status:         "locked",
		TargetChains:   targetChains,
		PairAccounts:   pairAccounts,
	}

	// Create bridge actions to establish pair accounts on target chains
	for _, chain := range targetChains {
		pairId := fmt.Sprintf("%s-%s", asset0, asset1)
		if asset0 > asset1 {
			pairId = fmt.Sprintf("%s-%s", asset1, asset0)
		}

		action := &dex.PairBridgeAction{
			Id:          fmt.Sprintf("create-%s-%s-%d", pairId, chain, creationHeight),
			Status:      "pending",
			PairId:      pairId,
			Chain:       chain,
			Direction:   "create",
			Asset:       "", // Not asset-specific for account creation
			Amount:      0,
			To:          pairAccounts[chain],
			BlockHeight: creationHeight,
			Memo:        fmt.Sprintf("Create DEX pair account for %s on %s", pairId, chain),
		}

		if err := r.dexDb.StorePairBridgeAction(action); err != nil {
			r.log.Debug("Failed to store pair bridge action", "error", err)
			// Don't fail the pool registration, just log the error
		}
	}

	return r.dexDb.SetPoolInfo(pool)
}

// ExecuteSwap orchestrates a two-hop swap through HBD
func (r *Router) ExecuteSwap(params dex.SwapParams) (*dex.SwapResult, error) {
	// Validate that we're not trying to swap the same asset
	if params.AssetIn == params.AssetOut {
		return &dex.SwapResult{
			Success:      false,
			ErrorMessage: "cannot swap asset to itself",
		}, nil
	}

	// If already swapping to/from HBD, do direct swap
	if params.AssetIn == "HBD" || params.AssetOut == "HBD" {
		return r.executeDirectSwap(params)
	}

	// Find pools for two-hop swap
	pool1, err := r.findPool(params.AssetIn, "HBD")
	if err != nil {
		return &dex.SwapResult{
			Success:      false,
			ErrorMessage: fmt.Sprintf("no pool found for %s/HBD: %v", params.AssetIn, err),
		}, nil
	}

	pool2, err := r.findPool("HBD", params.AssetOut)
	if err != nil {
		return &dex.SwapResult{
			Success:      false,
			ErrorMessage: fmt.Sprintf("no pool found for HBD/%s: %v", params.AssetOut, err),
		}, nil
	}

	// Calculate expected outputs and slippage bounds
	expectedHbdOut, err := r.calculateExpectedOutput(params.AmountIn, pool1.ContractId, params.AssetIn, "HBD")
	if err != nil {
		return &dex.SwapResult{
			Success:      false,
			ErrorMessage: fmt.Sprintf("failed to calculate HBD output: %v", err),
		}, nil
	}

	expectedFinalOut, err := r.calculateExpectedOutput(expectedHbdOut, pool2.ContractId, "HBD", params.AssetOut)
	if err != nil {
		return &dex.SwapResult{
			Success:      false,
			ErrorMessage: fmt.Sprintf("failed to calculate final output: %v", err),
		}, nil
	}

	// Calculate slippage bounds
	minHbdOut := r.calculateMinOutput(expectedHbdOut, params.MaxSlippage, params.MiddleOutRatio)
	minFinalOut := r.calculateMinOutput(expectedFinalOut, params.MaxSlippage, 1.0)

	// Use the higher of user-specified minimum or calculated minimum
	if params.MinAmountOut > minFinalOut {
		minFinalOut = params.MinAmountOut
	}

	// Execute first swap: AssetIn -> HBD
	firstSwapParams := dex.SwapParams{
		Sender:       params.Sender,
		AmountIn:     params.AmountIn,
		AssetIn:      params.AssetIn,
		AssetOut:     "HBD",
		MinAmountOut: minHbdOut,
		MaxSlippage:  params.MaxSlippage,
		Beneficiary:  params.Beneficiary,
		RefBps:       params.RefBps,
	}

	firstResult, err := r.executeDirectSwap(firstSwapParams)
	if err != nil || !firstResult.Success {
		return &dex.SwapResult{
			Success:      false,
			ErrorMessage: fmt.Sprintf("first swap failed: %v", err),
			Route:        []string{"hive"},
		}, nil
	}

	// Execute second swap: HBD -> AssetOut
	secondSwapParams := dex.SwapParams{
		Sender:       params.Sender,
		AmountIn:     firstResult.HbdAmount,
		AssetIn:      "HBD",
		AssetOut:     params.AssetOut,
		MinAmountOut: minFinalOut,
		MaxSlippage:  params.MaxSlippage,
		Beneficiary:  params.Beneficiary,
		RefBps:       params.RefBps,
	}

	secondResult, err := r.executeDirectSwap(secondSwapParams)
	if err != nil || !secondResult.Success {
		return &dex.SwapResult{
			Success:      false,
			ErrorMessage: fmt.Sprintf("second swap failed: %v", err),
		}, nil
	}

	return &dex.SwapResult{
		AmountOut: secondResult.AmountOut,
		HbdAmount: firstResult.HbdAmount,
		Fee0:      firstResult.Fee0,
		Fee1:      secondResult.Fee0,
		Route:     []string{"hive"}, // Single chain swap
		Success:   true,
	}, nil
}

// ExecuteCrossChainSwap orchestrates a swap that may span multiple chains
func (r *Router) ExecuteCrossChainSwap(params dex.SwapParams) (*dex.SwapResult, error) {
	// Determine if this needs cross-chain routing
	hivePools, err := r.dexDb.GetPoolsByAsset(params.AssetIn)
	if err != nil {
		return &dex.SwapResult{
			Success:      false,
			ErrorMessage: fmt.Sprintf("failed to find pools for %s: %v", params.AssetIn, err),
		}, nil
	}

	// Find pools that support cross-chain routing
	var crossChainPool *dex.PoolInfo
	for _, pool := range hivePools {
		if len(pool.TargetChains) > 0 {
			crossChainPool = &pool
			break
		}
	}

	// If no cross-chain pool found, fall back to regular swap
	if crossChainPool == nil {
		return r.ExecuteSwap(params)
	}

	// Check if target asset is available on other chains
	// For now, assume we want to route to Ethereum
	targetChain := "ethereum"
	if account, exists := crossChainPool.PairAccounts[targetChain]; exists {
		r.log.Debug("Routing cross-chain swap",
			"from_chain", "hive",
			"to_chain", targetChain,
			"pair_account", account)

		// Create bridge action for the swap amount
		pairId := fmt.Sprintf("%s-%s", params.AssetIn, params.AssetOut)
		if params.AssetIn > params.AssetOut {
			pairId = fmt.Sprintf("%s-%s", params.AssetOut, params.AssetIn)
		}

		action := &dex.PairBridgeAction{
			Id:          fmt.Sprintf("transfer-%s-%d", pairId, time.Now().Unix()),
			Status:      "pending",
			PairId:      pairId,
			Chain:       targetChain,
			Direction:   "transfer",
			Asset:       params.AssetIn,
			Amount:      params.AmountIn,
			From:        params.Sender,
			To:          account,
			BlockHeight: 0, // Will be set by caller
			Memo:        fmt.Sprintf("Cross-chain swap: %s → %s", params.AssetIn, params.AssetOut),
		}

		if err := r.dexDb.StorePairBridgeAction(action); err != nil {
			return &dex.SwapResult{
				Success:      false,
				ErrorMessage: fmt.Sprintf("failed to create bridge action: %v", err),
			}, nil
		}

		// For now, return success - in practice this would trigger the bridge
		return &dex.SwapResult{
			AmountOut: params.AmountIn, // Placeholder - would be calculated after bridge
			Success:   true,
			Route:     []string{"hive", targetChain},
		}, nil
	}

	// Fall back to single-chain swap
	return r.ExecuteSwap(params)
}

// executeDirectSwap executes a single swap between two assets
func (r *Router) executeDirectSwap(params dex.SwapParams) (*dex.SwapResult, error) {
	_, err := r.findPool(params.AssetIn, params.AssetOut)
	if err != nil {
		return nil, fmt.Errorf("no pool found for %s/%s: %v", params.AssetIn, params.AssetOut, err)
	}

	// Create contract call transaction
	// This would need to be implemented based on the actual contract calling mechanism
	// For now, return a placeholder result
	return &dex.SwapResult{
		AmountOut: params.AmountIn, // Placeholder
		Success:   true,
	}, nil
}

// findPool finds a pool for the given asset pair
func (r *Router) findPool(asset0, asset1 string) (*dex.PoolInfo, error) {
	pools, err := r.dexDb.GetPoolsByAsset(asset0)
	if err != nil {
		return nil, err
	}

	for _, pool := range pools {
		if (pool.Asset0 == asset0 && pool.Asset1 == asset1) ||
			(pool.Asset0 == asset1 && pool.Asset1 == asset0) {
			return &pool, nil
		}
	}

	return nil, fmt.Errorf("no pool found for %s/%s", asset0, asset1)
}

// calculateExpectedOutput calculates expected output for a swap
func (r *Router) calculateExpectedOutput(amountIn int64, contractId, assetIn, assetOut string) (int64, error) {
	// This would need to query the actual pool reserves from the contract
	// For now, return a placeholder calculation
	// In reality, this would call the contract to get current reserves and calculate output
	return amountIn, nil
}

// calculateMinOutput calculates minimum output based on slippage tolerance
func (r *Router) calculateMinOutput(expectedOut int64, maxSlippageBps uint64, ratio float64) int64 {
	slippageMultiplier := float64(maxSlippageBps) / 10000.0 * ratio
	minMultiplier := 1.0 - slippageMultiplier
	return int64(float64(expectedOut) * minMultiplier)
}

// UpdateBondingMetric updates the bonding curve metric for a pool
func (r *Router) UpdateBondingMetric(contractId string, newReserve0, newReserve1 uint64) error {
	return r.bondingCurveMgr.UpdateBondingMetric(contractId, newReserve0, newReserve1)
}

// GetPoolInfo returns pool information
func (r *Router) GetPoolInfo(contractId string) (*dex.PoolInfo, error) {
	return r.dexDb.GetPoolInfo(contractId)
}

// GetTokenRegistry returns the token registry
func (r *Router) GetTokenRegistry() *TokenRegistry {
	return r.tokenRegistry
}

// GetBondingCurveManager returns the bonding curve manager
func (r *Router) GetBondingCurveManager() *BondingCurveManager {
	return r.bondingCurveMgr
}

// RecordInitialLiquidity records the creator's initial LP token balance after first liquidity provision
// This enables bonding curve protection against immediate rug pulls
func (r *Router) RecordInitialLiquidity(contractId string, providerAddress string, lpAmount uint64) error {
	pool, err := r.dexDb.GetPoolInfo(contractId)
	if err != nil {
		return fmt.Errorf("failed to get pool info: %w", err)
	}

	// Only record if this is the pool creator and no initial liquidity recorded yet
	if providerAddress == pool.Creator && pool.LockedLPAmount == 0 {
		pool.LockedLPAmount = lpAmount
		r.log.Debug("Recorded initial liquidity for pool creator",
			"contract_id", contractId,
			"creator", providerAddress,
			"initial_lp", lpAmount)
		return r.dexDb.SetPoolInfo(pool)
	}

	return nil
}
