package tss

import (
	"errors"
	"fmt"

	"go.mongodb.org/mongo-driver/mongo"
)

// N2 — the EVM bridges' keysign halt, the BTC vault's (btcKeysignFrozen +
// btcSignGateDecision, solvency_gate.go / output_scoping.go) for the EVM
// vaults. A bridge's vault keysigns are halted while either
//   - governance froze the EVM bridges: vsc.tss_halt {"keyId":"evm"} from the
//     gateway multisig, mirrored in consensus state (EvmKeysignHalted; from
//     0.13.0, the line this gate runs on), or
//   - the bridge proved a theft: its "th" flag, set by the permissionless
//     reportUnauthorizedSpend when an account proof shows a vault address used
//     a nonce the bridge never authorized (account-mapping P1), deleted by the
//     owner's clearTheftHalt.
//
// "th" is read from the bridge's committed output at bh-1, not bh. The TSS
// tick for block bh runs while the state engine processes bh (consumer.go
// launches the async ticks, then ProcessBlock), so an output written in bh is
// visible to some nodes' ticks and not others; by bh every node has finished
// bh-1, so every node reads the same flag. A read that fails (other than the
// bridge having no output yet) refuses the sign: the same side as the halt.
//
// While halted, the node still lends its share where refusing would trap the
// funds the halt protects: a superseded generation's sweep to the active
// generation and a pending generation's check signature (BTC: V-8 and BRK-2),
// and a superseded generation's own in-flight withdrawals (scopeOwnWithdrawal,
// EVM only): its nonces below the ceiling must be mined before its sweep, which
// follows them, can be, and a re-sign or roll there re-signs only a
// transaction the bridge authorized before the cut-over.

// evmTheftHaltKey is the bridge contract's theft flag
// (account-mapping constants.TheftHaltKey). Any value halts; the contract
// clears by deleting it.
const evmTheftHaltKey = "th"

// evmKeysignHalted reports whether contract's vault keysigns are halted at bh.
// A non-nil error is a halt flag this node could not read; halted is then true.
func (tssMgr *TssManager) evmKeysignHalted(contract string, bh uint64) (bool, error) {
	if tssMgr.scheduler != nil && tssMgr.scheduler.EvmKeysignHalted() {
		return true, nil
	}
	if tssMgr.contractState == nil {
		return true, errors.New("contract state reader unavailable")
	}
	at := bh
	if at > 0 {
		at--
	}
	output, err := tssMgr.contractState.GetLastOutputStrict(contract, at)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return false, nil // the bridge has no output yet: nothing set
	}
	if err != nil {
		return true, fmt.Errorf("get last output for %s@%d: %w", contract, at, err)
	}
	raw, ok, err := tssMgr.stateKeyReader(output.StateMerkle)(evmTheftHaltKey)
	if err != nil {
		return true, err
	}
	return ok && len(raw) > 0, nil
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
