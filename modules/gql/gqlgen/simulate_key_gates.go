package gqlgen

import (
	"vsc-node/modules/common/consensusversion"
	"vsc-node/modules/common/params"
	contract_execution_context "vsc-node/modules/contract/execution-context"
	stateEngine "vsc-node/modules/state-processing"
)

// simulateTssKeyOpts returns the TssGetKey options a simulated call at
// blockHeight runs with: the same two "in force" rules TxVscCallContract.ExecuteTx
// applies (state-processing transactions.go), so a dry run reports the fields the
// chain would. Without the 0.11.0 one, every dry run of migrateVault read the
// active key as never reshared, and the mapping bot, which dry-runs the vault ops
// before sending them, waited on that refusal forever.
func simulateTssKeyOpts(cp params.ConsensusParams, blockHeight uint64, active consensusversion.Version) []contract_execution_context.Option {
	var opts []contract_execution_context.Option
	if stateEngine.VaultRotationV2InForce(cp, blockHeight, active) {
		opts = append(opts, contract_execution_context.WithVaultRotationV2(true))
	}
	if stateEngine.KeyCustodyInForce(cp, blockHeight, active) {
		opts = append(opts, contract_execution_context.WithKeyReshared(true))
	}
	return opts
}
