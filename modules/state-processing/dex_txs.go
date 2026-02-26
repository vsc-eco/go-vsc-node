//go:build dex
// +build dex

package state_engine

import (
	"fmt"
	contract_session "vsc-node/modules/contract/session"
	"vsc-node/modules/db/vsc/dex"
	ledgerSystem "vsc-node/modules/ledger-system"
	rcSystem "vsc-node/modules/rc-system"
)

// TxRegisterPool represents a transaction to register a new liquidity pool
type TxRegisterPool struct {
	Self         TxSelf
	Asset0       string   `json:"asset0"`
	Asset1       string   `json:"asset1"`
	ContractId   string   `json:"contract_id"`
	BaseFeeBps   uint64   `json:"base_fee_bps"`
	TargetChains []string `json:"target_chains,omitempty"` // Cross-chain support
}

func (tx TxRegisterPool) ExecuteTx(se *StateEngine, ledgerSession *LedgerSession, rcSession *rcSystem.RcSession, callSession *contract_session.CallSession, rcPayer string) TxResult {
	// Validate that pool creation fee has been paid
	params, err := se.dexDb.GetDexParams()
	if err != nil {
		return errorToTxResult(fmt.Errorf("failed to get DEX parameters: %w", err), 100)
	}

	// Check if user has paid the pool creation fee in HBD
	userBalance := ledgerSession.GetBalance(rcPayer, tx.Self.BlockHeight, "HBD")
	if userBalance < params.PoolCreationFee {
		return errorToTxResult(fmt.Errorf("insufficient HBD for pool creation fee. Required: %d, Available: %d", params.PoolCreationFee, userBalance), 100)
	}

	// Deduct pool creation fee
	feeTransfer := ledgerSystem.OpLogEvent{
		From:   rcPayer,
		To:     "system:fr_balance", // Send to system account
		Amount: params.PoolCreationFee,
		Asset:  "HBD",
		Type:   "transfer",
	}

	result := ledgerSession.ExecuteTransfer(feeTransfer)
	if !result.Ok {
		return errorToTxResult(fmt.Errorf("failed to deduct pool creation fee: %s", result.Msg), 100)
	}

	// Register the pool in the DEX router
	err = se.dexRouter.RegisterPool(tx.ContractId, tx.Asset0, tx.Asset1, rcPayer, tx.Self.BlockHeight, tx.TargetChains)
	if err != nil {
		return errorToTxResult(fmt.Errorf("failed to register pool: %w", err), 100)
	}

	se.log.Debug("Pool registered successfully",
		"contract_id", tx.ContractId,
		"assets", fmt.Sprintf("%s/%s", tx.Asset0, tx.Asset1),
		"target_chains", tx.TargetChains)

	return TxResult{
		Success: true,
		Ret:     fmt.Sprintf("Pool registered: %s", tx.ContractId),
		RcUsed:  100,
	}
}

func (tx TxRegisterPool) Type() string {
	return "register_pool"
}

func (tx TxRegisterPool) TxSelf() TxSelf {
	return tx.Self
}

func (tx TxRegisterPool) ToData() map[string]interface{} {
	data := map[string]interface{}{
		"type":         "register_pool",
		"asset0":       tx.Asset0,
		"asset1":       tx.Asset1,
		"contract_id":  tx.ContractId,
		"base_fee_bps": tx.BaseFeeBps,
	}
	if len(tx.TargetChains) > 0 {
		data["target_chains"] = tx.TargetChains
	}
	return data
}

// TxVscDexSwap represents a DEX swap transaction
type TxVscDexSwap struct {
	Self           TxSelf
	AmountIn       int64   `json:"amount_in"`
	AssetIn        string  `json:"asset_in"`
	AssetOut       string  `json:"asset_out"`
	MinAmountOut   int64   `json:"min_amount_out"`
	MaxSlippageBps uint64  `json:"max_slippage_bps"`
	MiddleOutRatio float64 `json:"middle_out_ratio"`
	Beneficiary    string  `json:"beneficiary"`
	RefBps         uint64  `json:"ref_bps"`
}

func (tx TxVscDexSwap) ExecuteTx(se *StateEngine, ledgerSession *LedgerSession, rcSession *rcSystem.RcSession, callSession *contract_session.CallSession, rcPayer string) TxResult {
	// Create swap parameters
	swapParams := dex.SwapParams{
		Sender:         rcPayer,
		AmountIn:       tx.AmountIn,
		AssetIn:        tx.AssetIn,
		AssetOut:       tx.AssetOut,
		MinAmountOut:   tx.MinAmountOut,
		MaxSlippage:    tx.MaxSlippageBps,
		MiddleOutRatio: tx.MiddleOutRatio,
		Beneficiary:    tx.Beneficiary,
		RefBps:         tx.RefBps,
	}

	// Execute the swap through the router
	result, err := se.dexRouter.ExecuteSwap(swapParams)
	if err != nil {
		return errorToTxResult(fmt.Errorf("swap execution failed: %w", err), 100)
	}

	if !result.Success {
		return TxResult{
			Success: false,
			Ret:     result.ErrorMessage,
			RcUsed:  100,
		}
	}

	se.log.Debug("DEX swap executed successfully",
		"asset_in", tx.AssetIn,
		"asset_out", tx.AssetOut,
		"amount_in", tx.AmountIn,
		"amount_out", result.AmountOut)

	return TxResult{
		Success: true,
		Ret:     fmt.Sprintf("Swap successful: %d %s -> %d %s", tx.AmountIn, tx.AssetIn, result.AmountOut, tx.AssetOut),
		RcUsed:  100,
	}
}

func (tx TxVscDexSwap) Type() string {
	return "vsc_dex_swap"
}

func (tx TxVscDexSwap) TxSelf() TxSelf {
	return tx.Self
}

func (tx TxVscDexSwap) ToData() map[string]interface{} {
	return map[string]interface{}{
		"type":             "vsc_dex_swap",
		"amount_in":        tx.AmountIn,
		"asset_in":         tx.AssetIn,
		"asset_out":        tx.AssetOut,
		"min_amount_out":   tx.MinAmountOut,
		"max_slippage_bps": tx.MaxSlippageBps,
		"middle_out_ratio": tx.MiddleOutRatio,
		"beneficiary":      tx.Beneficiary,
		"ref_bps":          tx.RefBps,
	}
}

// TxVscDexCrossChainSwap represents a cross-chain DEX swap transaction
type TxVscDexCrossChainSwap struct {
	Self           TxSelf
	AmountIn       int64   `json:"amount_in"`
	AssetIn        string  `json:"asset_in"`
	AssetOut       string  `json:"asset_out"`
	MinAmountOut   int64   `json:"min_amount_out"`
	MaxSlippageBps uint64  `json:"max_slippage_bps"`
	MiddleOutRatio float64 `json:"middle_out_ratio"`
	Beneficiary    string  `json:"beneficiary"`
	RefBps         uint64  `json:"ref_bps"`
	TargetChain    string  `json:"target_chain"` // "ethereum", "polygon", etc.
}

func (tx TxVscDexCrossChainSwap) ExecuteTx(se *StateEngine, ledgerSession *LedgerSession, rcSession *rcSystem.RcSession, callSession *contract_session.CallSession, rcPayer string) TxResult {
	// Create swap parameters
	swapParams := dex.SwapParams{
		Sender:         rcPayer,
		AmountIn:       tx.AmountIn,
		AssetIn:        tx.AssetIn,
		AssetOut:       tx.AssetOut,
		MinAmountOut:   tx.MinAmountOut,
		MaxSlippage:    tx.MaxSlippageBps,
		MiddleOutRatio: tx.MiddleOutRatio,
		Beneficiary:    tx.Beneficiary,
		RefBps:         tx.RefBps,
	}

	// Execute the cross-chain swap through the router
	result, err := se.dexRouter.ExecuteCrossChainSwap(swapParams)
	if err != nil {
		return errorToTxResult(fmt.Errorf("cross-chain swap execution failed: %w", err), 100)
	}

	if !result.Success {
		return TxResult{
			Success: false,
			Ret:     result.ErrorMessage,
			RcUsed:  100,
		}
	}

	se.log.Debug("Cross-chain DEX swap executed successfully",
		"asset_in", tx.AssetIn,
		"asset_out", tx.AssetOut,
		"amount_in", tx.AmountIn,
		"amount_out", result.AmountOut,
		"route", result.Route,
		"target_chain", tx.TargetChain)

	return TxResult{
		Success: true,
		Ret:     fmt.Sprintf("Cross-chain swap successful: %d %s -> %d %s via %v", tx.AmountIn, tx.AssetIn, result.AmountOut, tx.AssetOut, result.Route),
		RcUsed:  100,
	}
}

func (tx TxVscDexCrossChainSwap) Type() string {
	return "vsc_dex_cross_chain_swap"
}

func (tx TxVscDexCrossChainSwap) TxSelf() TxSelf {
	return tx.Self
}

func (tx TxVscDexCrossChainSwap) ToData() map[string]interface{} {
	return map[string]interface{}{
		"type":             "vsc_dex_cross_chain_swap",
		"amount_in":        tx.AmountIn,
		"asset_in":         tx.AssetIn,
		"asset_out":        tx.AssetOut,
		"min_amount_out":   tx.MinAmountOut,
		"max_slippage_bps": tx.MaxSlippageBps,
		"middle_out_ratio": tx.MiddleOutRatio,
		"beneficiary":      tx.Beneficiary,
		"ref_bps":          tx.RefBps,
		"target_chain":     tx.TargetChain,
	}
}

// TxSetDexParams represents a transaction to update DEX parameters (governance only)
type TxSetDexParams struct {
	Self                  TxSelf
	PoolCreationFee       *int64  `json:"pool_creation_fee,omitempty"`
	BondingCurveThreshold *uint64 `json:"bonding_curve_threshold,omitempty"`
}

func (tx TxSetDexParams) ExecuteTx(se *StateEngine, ledgerSession *LedgerSession, rcSession *rcSystem.RcSession, callSession *contract_session.CallSession, rcPayer string) TxResult {
	// Check if caller is system/governance
	if !isSystemSender(rcPayer) {
		return errorToTxResult(fmt.Errorf("only system governance can update DEX parameters"), 100)
	}

	// Get current parameters
	currentParams, err := se.dexDb.GetDexParams()
	if err != nil {
		return errorToTxResult(fmt.Errorf("failed to get current DEX parameters: %w", err), 100)
	}

	// Update parameters if provided
	if tx.PoolCreationFee != nil {
		if *tx.PoolCreationFee < 0 {
			return errorToTxResult(fmt.Errorf("pool creation fee cannot be negative"), 100)
		}
		currentParams.PoolCreationFee = *tx.PoolCreationFee
	}

	if tx.BondingCurveThreshold != nil {
		if *tx.BondingCurveThreshold == 0 {
			return errorToTxResult(fmt.Errorf("bonding curve threshold cannot be zero"), 100)
		}
		currentParams.BondingCurveThreshold = *tx.BondingCurveThreshold
	}

	// Save updated parameters
	err = se.dexDb.SetDexParams(currentParams)
	if err != nil {
		return errorToTxResult(fmt.Errorf("failed to update DEX parameters: %w", err), 100)
	}

	se.log.Debug("DEX parameters updated",
		"pool_creation_fee", currentParams.PoolCreationFee,
		"bonding_curve_threshold", currentParams.BondingCurveThreshold)

	return TxResult{
		Success: true,
		Ret:     "DEX parameters updated successfully",
		RcUsed:  100,
	}
}

func (tx TxSetDexParams) Type() string {
	return "set_dex_params"
}

func (tx TxSetDexParams) TxSelf() TxSelf {
	return tx.Self
}

func (tx TxSetDexParams) ToData() map[string]interface{} {
	data := map[string]interface{}{
		"type": "set_dex_params",
	}
	if tx.PoolCreationFee != nil {
		data["pool_creation_fee"] = *tx.PoolCreationFee
	}
	if tx.BondingCurveThreshold != nil {
		data["bonding_curve_threshold"] = *tx.BondingCurveThreshold
	}
	return data
}

// Helper function to check if sender is system/governance
func isSystemSender(sender string) bool {
	return sender == "system:governance" || sender == "system:fr_balance"
}
