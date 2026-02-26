package gateway

import (
	"fmt"
	"vsc-node/lib/logger"
	"vsc-node/modules/db/vsc/dex"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	dex_router "vsc-node/modules/dex-router"
	stateEngine "vsc-node/modules/state-processing"
)

// DexBridgeManager handles DEX-specific bridge operations
type DexBridgeManager struct {
	dexDb     dex.DexDb
	dexRouter *dex_router.Router
	ledgerDb  ledgerDb.BridgeActions
	log       logger.Logger
	se        *stateEngine.StateEngine
}

// NewDexBridgeManager creates a new DEX bridge manager
func NewDexBridgeManager(dexDb dex.DexDb, dexRouter *dex_router.Router, ledgerDb ledgerDb.BridgeActions, log logger.Logger, se *stateEngine.StateEngine) *DexBridgeManager {
	return &DexBridgeManager{
		dexDb:     dexDb,
		dexRouter: dexRouter,
		ledgerDb:  ledgerDb,
		log:       log,
		se:        se,
	}
}

// ProcessPendingPairActions processes pending DEX pair bridge actions
func (dbm *DexBridgeManager) ProcessPendingPairActions(chain string, bh uint64) error {
	actions, err := dbm.dexDb.GetPendingPairBridgeActions(chain)
	if err != nil {
		return fmt.Errorf("failed to get pending pair actions: %w", err)
	}

	for _, action := range actions {
		if err := dbm.processPairAction(action, bh); err != nil {
			dbm.log.Debug("Failed to process pair action", "action_id", action.Id, "error", err)
			continue
		}
	}

	return nil
}

// processPairAction processes a single DEX pair bridge action
func (dbm *DexBridgeManager) processPairAction(action dex.PairBridgeAction, bh uint64) error {
	switch action.Direction {
	case "create":
		return dbm.createPairAccount(action, bh)
	case "transfer":
		return dbm.transferToPairAccount(action, bh)
	case "withdraw":
		return dbm.withdrawFromPairAccount(action, bh)
	default:
		return fmt.Errorf("unknown action direction: %s", action.Direction)
	}
}

// createPairAccount creates a DEX pair account on the target chain
func (dbm *DexBridgeManager) createPairAccount(action dex.PairBridgeAction, bh uint64) error {
	dbm.log.Debug("Creating DEX pair account",
		"pair_id", action.PairId,
		"chain", action.Chain,
		"account", action.To)

	// In a real implementation, this would interact with the target chain
	// For now, we just mark the action as complete
	return dbm.dexDb.UpdatePairBridgeActionStatus(action.Id, "complete")
}

// transferToPairAccount transfers assets to a DEX pair account
func (dbm *DexBridgeManager) transferToPairAccount(action dex.PairBridgeAction, bh uint64) error {
	dbm.log.Debug("Transferring to DEX pair account",
		"pair_id", action.PairId,
		"chain", action.Chain,
		"asset", action.Asset,
		"amount", action.Amount,
		"from", action.From,
		"to", action.To)

	// Create a bridge action record for the ledger system
	bridgeAction := ledgerDb.ActionRecord{
		Id:          action.Id,
		Status:      "pending",
		Amount:      action.Amount,
		Asset:       action.Asset,
		To:          action.To,
		Memo:        fmt.Sprintf("DEX pair transfer: %s", action.PairId),
		TxId:        action.TxId,
		Type:        "bridge_transfer",
		BlockHeight: bh,
	}

	// Store the bridge action
	dbm.ledgerDb.StoreAction(bridgeAction)

	// Mark the DEX pair action as complete
	return dbm.dexDb.UpdatePairBridgeActionStatus(action.Id, "complete")
}

// withdrawFromPairAccount withdraws assets from a DEX pair account
func (dbm *DexBridgeManager) withdrawFromPairAccount(action dex.PairBridgeAction, bh uint64) error {
	dbm.log.Debug("Withdrawing from DEX pair account",
		"pair_id", action.PairId,
		"chain", action.Chain,
		"asset", action.Asset,
		"amount", action.Amount,
		"from", action.From,
		"to", action.To)

	// Create a bridge action record for the ledger system
	bridgeAction := ledgerDb.ActionRecord{
		Id:          action.Id,
		Status:      "pending",
		Amount:      action.Amount,
		Asset:       action.Asset,
		To:          action.To,
		Memo:        fmt.Sprintf("DEX pair withdrawal: %s", action.PairId),
		TxId:        action.TxId,
		Type:        "bridge_withdrawal",
		BlockHeight: bh,
	}

	// Store the bridge action
	dbm.ledgerDb.StoreAction(bridgeAction)

	// Mark the DEX pair action as complete
	return dbm.dexDb.UpdatePairBridgeActionStatus(action.Id, "complete")
}

// GetPairAccountBalance gets the balance of a DEX pair account on a specific chain
func (dbm *DexBridgeManager) GetPairAccountBalance(pairId, chain, asset string) (int64, error) {
	// In a real implementation, this would query the target chain
	// For now, return 0
	return 0, nil
}

// ValidatePairAccount checks if a DEX pair account exists on a specific chain
func (dbm *DexBridgeManager) ValidatePairAccount(pairId, chain, account string) error {
	pool, err := dbm.dexDb.GetPoolInfo("") // Would need to find pool by pairId
	if err != nil {
		return fmt.Errorf("pool not found for pair %s", pairId)
	}

	if pairAccount, exists := pool.PairAccounts[chain]; !exists || pairAccount != account {
		return fmt.Errorf("pair account %s not found for chain %s", account, chain)
	}

	return nil
}
