package dex_router

import (
	"fmt"
	"math"
	"vsc-node/lib/logger"
	"vsc-node/modules/db/vsc/dex"
)

type BondingCurveManager struct {
	dexDb dex.DexDb
	log   logger.Logger
}

func NewBondingCurveManager(dexDb dex.DexDb, log logger.Logger) *BondingCurveManager {
	return &BondingCurveManager{
		dexDb: dexDb,
		log:   log,
	}
}

// UpdateBondingMetric updates the bonding curve metric for a pool after liquidity changes
func (bcm *BondingCurveManager) UpdateBondingMetric(contractId string, newReserve0, newReserve1 uint64) error {
	// Calculate new metric: sqrt(reserve0 * reserve1)
	metric := uint64(math.Sqrt(float64(newReserve0 * newReserve1)))

	// Update the metric in the database
	err := bcm.dexDb.UpdateBondingMetric(contractId, metric)
	if err != nil {
		return fmt.Errorf("failed to update bonding metric: %w", err)
	}

	// Check if the pool should be unlocked
	pool, err := bcm.dexDb.GetPoolInfo(contractId)
	if err != nil {
		return fmt.Errorf("failed to get pool info: %w", err)
	}

	if pool.Status == "locked" && metric >= pool.BondingTarget {
		bcm.log.Debug("Pool bonding target reached",
			"contract_id", contractId,
			"metric", metric,
			"target", pool.BondingTarget)

		// The pool will be automatically unlocked by the database update
		// This is handled in the UpdateBondingMetric method in dex.go
	}

	return nil
}

// GetBondingProgress returns the current bonding progress for a pool
func (bcm *BondingCurveManager) GetBondingProgress(contractId string) (float64, error) {
	pool, err := bcm.dexDb.GetPoolInfo(contractId)
	if err != nil {
		return 0, fmt.Errorf("failed to get pool info: %w", err)
	}

	if pool.BondingTarget == 0 {
		return 1.0, nil // Already unlocked or no target set
	}

	progress := float64(pool.BondingMetric) / float64(pool.BondingTarget)
	if progress > 1.0 {
		progress = 1.0
	}

	return progress, nil
}

// IsPoolUnlocked checks if a pool has reached its bonding target
func (bcm *BondingCurveManager) IsPoolUnlocked(contractId string) (bool, error) {
	pool, err := bcm.dexDb.GetPoolInfo(contractId)
	if err != nil {
		return false, fmt.Errorf("failed to get pool info: %w", err)
	}

	return pool.Status == "unlocked", nil
}

// GetCreatorWithdrawalLimit returns how much of the creator's LP tokens can be withdrawn
// based on bonding curve progress. This prevents rug pulls while allowing organic withdrawals.
func (bcm *BondingCurveManager) GetCreatorWithdrawalLimit(contractId string, creatorAddress string) (float64, error) {
	pool, err := bcm.dexDb.GetPoolInfo(contractId)
	if err != nil {
		return 0, fmt.Errorf("failed to get pool info: %w", err)
	}

	// If pool is unlocked, creator can withdraw 100%
	if pool.Status == "unlocked" {
		return 1.0, nil
	}

	// If no bonding target set, allow full withdrawal (backwards compatibility)
	if pool.BondingTarget == 0 {
		return 1.0, nil
	}

	// Creator can only withdraw proportionally to bonding curve progress
	progress := float64(pool.BondingMetric) / float64(pool.BondingTarget)
	if progress > 1.0 {
		progress = 1.0
	}

	// Minimum withdrawal limit to prevent complete lockout
	// Creator can always withdraw at least 10% of their initial LP tokens,
	// but rug pulls are still limited to this amount until bonding curve matures
	if progress < 0.1 { // At least 10% can always be withdrawn
		progress = 0.1
	}

	return progress, nil
}

// CanCreatorWithdraw checks if the creator can withdraw a specific amount of LP tokens
func (bcm *BondingCurveManager) CanCreatorWithdraw(contractId string, creatorAddress string, withdrawAmount uint64, totalCreatorLP uint64) (bool, error) {
	if totalCreatorLP == 0 {
		return true, nil // No creator LP tokens, so no restriction
	}

	limit, err := bcm.GetCreatorWithdrawalLimit(contractId, creatorAddress)
	if err != nil {
		return false, err
	}

	maxWithdraw := uint64(float64(totalCreatorLP) * limit)
	return withdrawAmount <= maxWithdraw, nil
}

// GetLockedLPAmount returns the amount of creator LP tokens that are still locked
// (for backwards compatibility - this represents tokens the creator can't withdraw yet)
func (bcm *BondingCurveManager) GetLockedLPAmount(contractId string) (uint64, error) {
	pool, err := bcm.dexDb.GetPoolInfo(contractId)
	if err != nil {
		return 0, fmt.Errorf("failed to get pool info: %w", err)
	}

	if pool.Status == "unlocked" {
		return 0, nil // No LP tokens are locked if pool is unlocked
	}

	// Return the creator's locked amount (for display purposes)
	return pool.LockedLPAmount, nil
}

// SetLockedLPAmount sets the amount of LP tokens to be locked for a pool
func (bcm *BondingCurveManager) SetLockedLPAmount(contractId string, amount uint64) error {
	pool, err := bcm.dexDb.GetPoolInfo(contractId)
	if err != nil {
		return fmt.Errorf("failed to get pool info: %w", err)
	}

	pool.LockedLPAmount = amount
	return bcm.dexDb.SetPoolInfo(pool)
}
