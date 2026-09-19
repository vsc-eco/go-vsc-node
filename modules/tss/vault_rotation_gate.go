package tss

import (
	"vsc-node/modules/common/consensusversion"
)

// vaultRotationV2InForce reports whether the BTC vault-rotation-v2 batch is in
// force at bh. It is the TSS-side form of state-processing's
// VaultRotationV2InForce (modules/state-processing/consensus_version.go) and is
// the ONLY gate the TSS half of the batch may read: the reshare skip, the S3
// output-scoping gate, and the retiring-gen readiness set all key off it, so the
// TSS side can never lag the contract-execution side once the floor rises.
//
// In force when EITHER:
//   - the explicit ConsensusParams.VaultRotationV2ActivationHeight pin covers
//     bh (the ephemeral/devnet path; a set pin short-circuits so a pinned
//     network never pays the election read), or
//   - the chain-active consensus version at bh — the on-chain election's
//     version floor (GetScheduler.TssMinimumConsensusVersion ==
//     StateEngine.ActiveConsensusVersion) — has reached
//     consensusversion.V0_8_0 (the mainnet path, where the pin must stay 0 so
//     the attested floor is the only intended activation path).
//
// Determinism: both inputs are height-addressable on-chain/config values (the
// pin is a pure function of config + height; the floor is the committed
// election version at bh), so every node resolves the identical verdict — the
// same coordinated-activation property the contract-execution half relies on.
//
// Fails INERT on uncertainty: nil sconf, or a nil scheduler (test/standalone
// construction), resolves pin-only. The floor path only ADDS activation, so the
// fallback is fail-safe.
func (tssMgr *TssManager) vaultRotationV2InForce(bh uint64) bool {
	if tssMgr.sconf == nil {
		return false
	}
	if tssMgr.sconf.ConsensusParams().VaultRotationV2Enabled(bh) {
		return true
	}
	if tssMgr.scheduler == nil {
		return false
	}
	return consensusversion.VaultRotationV2Active(tssMgr.scheduler.TssMinimumConsensusVersion(bh))
}
