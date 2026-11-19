package tss

// N2 — the EVM bridges' keysign halt, the BTC vault's (btcKeysignFrozen +
// btcSignGateDecision, solvency_gate.go / output_scoping.go) for the EVM
// vaults. A bridge's vault keysigns are halted while either
//   - governance froze the EVM bridges: vsc.tss_halt {"keyId":"evm"} from the
//     gateway multisig, mirrored in consensus state (EvmKeysignHalted; from
//     0.13.0, the line this gate runs on), or
//   - the bridge proved a theft: its "th" flag, set by the permissionless
//     reportUnauthorizedSpend when an account proof shows a vault address used
//     a nonce the bridge never authorized (account-mapping P1), deleted by the
//     owner's clearTheftHalt; or
//   - the bridge's custody proved short: its "si" flag, set by the
//     permissionless proveCustody when the ETH proven at every fund-holding
//     generation's address is below the booked custody (account-mapping C2),
//     deleted by a later full proof or the owner's setCustodyCheck off.
//
// Every flag is read from the consensus cache with no I/O (CG-1). "th" and
// "si" are mirrored there by the state engine at the end of each block
// (state-processing refreshEvmBridgeHalts), from the bridge's last output at
// or below that block. The TSS tick for bh starts after block bh-1 is
// processed, so every node reads them at least as of bh-1, as the old
// sign-time read did; a node whose tick reaches this gate after it has also
// processed bh reads them as of bh (a flag that changes in bh can fail that
// one session, retried next round). They used to be read from Mongo and the datalayer here, and a failed
// read counted as a halt: EVM vault signing needs every listed signer, so one
// node's read error stopped every EVM vault signature. The mirror keeps a
// bridge's last value when its read fails instead.
//
// While halted, the node still lends its share where refusing would trap the
// funds the halt protects: a superseded generation's sweep to the active
// generation and a pending generation's check signature (BTC: V-8 and BRK-2),
// and a superseded generation's own in-flight withdrawals (scopeOwnWithdrawal,
// EVM only): its nonces below the ceiling must be mined before its sweep, which
// follows them, can be, and a re-sign or roll there re-signs only a
// transaction the bridge authorized before the cut-over.

// evmKeysignHalted reports whether contract's vault keysigns are halted.
func (tssMgr *TssManager) evmKeysignHalted(contract string) bool {
	if tssMgr.scheduler == nil {
		return true // unreachable: evmRotatingKey needs a scheduler
	}
	return tssMgr.scheduler.EvmKeysignHalted() || tssMgr.scheduler.EvmBridgeHalted(contract)
}

// evmSignGateDecision combines the scope verdict with the halt (true = skip
// issuance). Pure, so the exemptions are unit-testable.
func evmSignGateDecision(verdict scopeVerdict, halted bool) bool {
	if verdict == scopeRefuse {
		return true
	}
	if halted && verdict != scopeSuccessorSweep && verdict != scopeCheckSig && verdict != scopeOwnWithdrawal {
		return true
	}
	return false
}
