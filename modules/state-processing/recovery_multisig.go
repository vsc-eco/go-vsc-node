package state_engine

import (
	"strings"

	"vsc-node/modules/common/params"
)

// RecoveryMultisigConfigured reports whether a usable recovery multisig roster
// exists at all: a non-empty account list with a positive threshold that the
// roster can actually satisfy. When this is false, vsc.recovery_suspend and
// vsc.recovery_require_version can NEVER take effect — the chain has no on-chain
// emergency stop (GV-H5). It is deliberately distinct from "configured but the
// signer set is unauthorized", so callers can tell a governance MISCONFIGURATION
// (no roster) apart from a legitimately rejected attempt.
func RecoveryMultisigConfigured(p params.ConsensusParams) bool {
	return len(p.RecoveryMultisigAccounts) > 0 &&
		p.RecoveryMultisigThreshold > 0 &&
		p.RecoveryMultisigThreshold <= len(p.RecoveryMultisigAccounts)
}

// VerifyRecoveryMultisig returns true if distinct required_auths accounts that appear in
// p.RecoveryMultisigAccounts meet or exceed p.RecoveryMultisigThreshold.
func VerifyRecoveryMultisig(p params.ConsensusParams, requiredAuths []string) bool {
	if len(p.RecoveryMultisigAccounts) == 0 || p.RecoveryMultisigThreshold <= 0 {
		return false
	}
	allowed := make(map[string]bool, len(p.RecoveryMultisigAccounts))
	for _, a := range p.RecoveryMultisigAccounts {
		allowed[strings.TrimPrefix(a, "hive:")] = true
	}
	if p.RecoveryMultisigThreshold > len(allowed) {
		return false
	}
	seen := make(map[string]bool)
	for _, raw := range requiredAuths {
		acc := strings.TrimPrefix(raw, "hive:")
		if allowed[acc] && !seen[acc] {
			seen[acc] = true
		}
	}
	return len(seen) >= p.RecoveryMultisigThreshold
}
