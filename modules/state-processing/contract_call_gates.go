package state_engine

import (
	"vsc-node/modules/common/consensusversion"
	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"
	contract_execution_context "vsc-node/modules/contract/execution-context"
)

// ContractCallGates is every option of a contract call that depends on the
// chain-active consensus version or on the network's configuration, for a call
// at blockHeight. The chain (TxVscCallContract.ExecuteTx) and the dry run (gql
// simulateContractCalls) both build their execution context from this one
// list, so a rule added here reaches both.
//
// It exists because they drifted, twice: the dry run never got the 0.11.0
// key-reshared field (2026-10-13: the mapping bot dry-runs migrateVault and
// would have waited for ever), and it never got try/catch or the TSS key
// allowlist (SIM-TRY-1, 2026-11-17: a dry run of a deposit whose swap fails
// aborted where the real call catches the failure and credits the deposit, so
// the bots sized such a call wrongly).
//
// Each gate, as it was written at the call site:
//   - try/catch inter-contract calls: from TryCatchICCVersion, so the new
//     semantics start at a coordinated version floor;
//   - BRK-2 (D1): TssGetKey's check-signature field, in force by the
//     vault-rotation-v2 rules (a height pin or the attested 0.8.0 floor). It
//     feeds hashed contract state: when off the field is absent, not empty;
//   - 0.11.0: TssGetKey's reshared field, gated the same way;
//   - 0.10.0: SP1 proof checks priced by work;
//   - 0.13.0: big swap amounts (SP-0d);
//   - 0.14.0 KEY-SPAM-1 (18j, TssKeyAllowlistActive): tss.create_key only
//     for the contracts the network lists; nil before, so create_key is
//     byte-identical until then;
//   - 0.15.0: the price of a new stored byte (params.WriteIoGasRcCost), 19 RC
//     below the line and 1 RC from it. It decides how much RC a call uses, so
//     a dry run on another price would size limits the chain then overruns or
//     never needs;
//   - 0.16.0: one stored value and key are bounded
//     (params.MAX_STATE_VALUE_BYTES, MAX_STATE_KEY_BYTES), so the dry run
//     fails a write the chain refuses;
//   - 0.16.0: a contract reached through contracts.call is held to the
//     caller's TSS key allowlist (TssKeyAllowlistNestedActive).
//
// A nil sconf (a state engine without a system config) leaves the two
// config-dependent gates off and the allowlist nil, as before.
func ContractCallGates(sconf systemconfig.SystemConfig, blockHeight uint64, activeVersion consensusversion.Version) []contract_execution_context.Option {
	var cp params.ConsensusParams
	if sconf != nil {
		cp = sconf.ConsensusParams()
	}
	return ContractCallGatesFor(sconf != nil, cp, tssKeyAllowlistFor(sconf, blockHeight, activeVersion), blockHeight, activeVersion)
}

// ContractCallGatesFor is ContractCallGates on the values it reads from the
// system config (haveConf: there is one), for tests.
func ContractCallGatesFor(haveConf bool, cp params.ConsensusParams, tssKeyAllowlist []string, blockHeight uint64, activeVersion consensusversion.Version) []contract_execution_context.Option {
	return []contract_execution_context.Option{
		contract_execution_context.WithTryCatch(activeVersion.MeetsConsensusMin(consensusversion.TryCatchICCVersion)),
		contract_execution_context.WithVaultRotationV2(haveConf && VaultRotationV2InForce(cp, blockHeight, activeVersion)),
		contract_execution_context.WithKeyReshared(haveConf && KeyCustodyInForce(cp, blockHeight, activeVersion)),
		contract_execution_context.WithSp1WorkPricing(consensusversion.Sp1WorkPricingActive(activeVersion)),
		contract_execution_context.WithBigSwapAmounts(consensusversion.BigSwapAmountsActive(activeVersion)),
		contract_execution_context.WithTssKeyAllowlist(tssKeyAllowlist),
		contract_execution_context.WithIoWriteRcCost(params.WriteIoGasRcCost(activeVersion)),
		contract_execution_context.WithStateSizeBound(consensusversion.SlotObjectSizeBoundActive(activeVersion)),
		contract_execution_context.WithTssKeyAllowlistNested(consensusversion.TssKeyAllowlistNestedActive(activeVersion)),
	}
}

// ContractCallRcCharge is the RC a contract call is charged: the gas it used,
// rounded up to whole RC, never under the 100 RC minimum.
//
// budgetRc is what the call was given to spend, min(the payer's available RC,
// the call's rc_limit). From 0.15.0 (RcChargeCappedAtLimitActive) the charge
// never exceeds it. The gas a call reports can: a storage operation is charged
// after it ran, so the one that takes the call past its budget is counted in
// full before the call is stopped. Below 0.15.0 that whole amount is charged,
// as it always was.
//
// The chain (TxVscCallContract.ExecuteTx) and the dry run both charge through
// this one function.
func ContractCallRcCharge(gasUsed uint, budgetRc uint64, activeVersion consensusversion.Version) int64 {
	// Integer ceil-divide: (gas + denom - 1) / denom. uint64 stays well below
	// overflow for any realistic gas value (denom = 100_000).
	rc := (uint64(gasUsed) + params.CYCLE_GAS_PER_RC - 1) / params.CYCLE_GAS_PER_RC
	if consensusversion.RcChargeCappedAtLimitActive(activeVersion) && rc > budgetRc {
		rc = budgetRc
	}
	if rc < 100 {
		rc = 100
	}
	return int64(rc)
}
