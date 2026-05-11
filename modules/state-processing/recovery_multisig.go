package state_engine

import (
	"strings"

	"vsc-node/modules/common/params"
)

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
