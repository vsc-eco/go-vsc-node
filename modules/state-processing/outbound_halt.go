package state_engine

import (
	"context"

	"vsc-node/modules/common/common_types"
	"vsc-node/modules/common/consensusversion"
	"vsc-node/modules/db/vsc/consensus_state"
	governance "vsc-node/modules/governance"
)

// haltCooled reports whether an outbound-halt entry is fully past its per-node
// cooldown at bh and may be pruned (it is inactive AND no longer blocks its
// setter from re-halting). Shared by executeHalt and executeUnhalt so the
// cooldown/prune predicate lives in one place.
func haltCooled(h consensus_state.OutboundHalt, bh uint64) bool {
	return h.SetHeight+HaltCooldownBlocks <= bh
}

// exitFrozen is the one-line guard the user-exit handlers use (v0.6.0 vault
// protections, brief fix 2): true when a halt is active at height AND the 0.6.0
// floor is in force, so an exit op (withdraw / unstake / consensus_unstake)
// must be rejected to trap collateral for the response window. Below the floor
// it is always false, keeping pre-activation replay byte-identical.
func exitFrozen(se common_types.StateEngine, height uint64) bool {
	return consensusversion.OutboundHaltActive(se.ActiveConsensusVersion(height)) && se.ExitsFrozenAt(height)
}

// Outbound-halt window bounds (v0.6.0 vault protections, brief fix 3). Hive
// blocks are ~3s, so 1200 blocks ≈ 1 hour. These are consensus-deterministic
// constants (every node clamps identically); they are candidates to migrate to
// ConsensusParams if governance wants them network-tunable.
const (
	// HaltDefaultBlocks is the window applied when a vsc.halt op omits Duration.
	HaltDefaultBlocks = uint64(1200) // ~1h
	// HaltMaxBlocks caps a SINGLE node's halt window (anti-grief). One node can
	// freeze outbounds only this long; a sustained freeze needs multiple honest
	// validators to stack — exactly the intended minority protection.
	HaltMaxBlocks = uint64(1200) // ~1h
	// HaltMinBlocks floors the window so a halt is always meaningful.
	HaltMinBlocks = uint64(20) // ~1min (one ACTION_INTERVAL)
	// HaltCooldownBlocks is the per-node rate limit: an account may set at most one
	// halt per cooldown. Set to 2x HaltMaxBlocks so a single node cannot freeze
	// outbounds more than ~50% of the time (anti-grief), while still letting a
	// solvency auto-halt re-engage ~1h after its window expires; honest nodes stack
	// to cover the gaps. Deactivated entries are RETAINED until they pass cooldown,
	// so an early-un-halted griefer's cooldown keeps running (no immediate re-halt).
	HaltCooldownBlocks = 2 * HaltMaxBlocks // ~2h
)

// executeHalt sets a bounded single-node outbound halt (brief fix 3). Authorized
// by ANY ONE current committee member (not a 2/3 threshold): the minority must
// be able to stop outbounds even when outvoted on signing. Multiple validators
// stack (union of active windows), and the per-node cooldown bounds griefing.
func (se *StateEngine) executeHalt(tx *TxHalt) {
	bh := tx.Self.BlockHeight
	// Inert below the 0.6.0 floor: pre-activation replay never consults or writes
	// halt state, so historical gateway behavior is byte-identical.
	if !consensusversion.OutboundHaltActive(se.ActiveConsensusVersion(bh)) {
		return
	}
	if se.consensusState == nil {
		return
	}

	// Auth: the signer must be a member of the committee active at this height.
	electorate, ok := se.governanceElectorate(bh)
	if !ok {
		log.Warn("vsc.halt ignored: no election covers height", "txId", tx.Self.TxId, "height", bh)
		return
	}
	setter := ""
	for _, auth := range tx.Self.RequiredAuths {
		na := governance.NormalizeAccount(auth)
		if governanceIsMember(electorate, na) {
			setter = na
			break
		}
	}
	if setter == "" {
		log.Warn("vsc.halt rejected: signer is not a current committee member",
			"txId", tx.Self.TxId, "requiredAuths", tx.Self.RequiredAuths)
		return
	}

	st, err := se.consensusState.Get(context.Background())
	if err != nil {
		log.Warn("vsc.halt: consensus state read failed", "err", err)
		return
	}

	// Rebuild the set: drop fully-cooled entries, reject if this setter is still
	// within its cooldown, keep the rest.
	kept := make([]consensus_state.OutboundHalt, 0, len(st.OutboundHalts)+1)
	for _, h := range st.OutboundHalts {
		if haltCooled(h, bh) {
			continue // fully cooled — prune
		}
		if h.Account == setter {
			log.Warn("vsc.halt rejected: setter within its per-node cooldown",
				"account", setter, "lastSetHeight", h.SetHeight,
				"cooldownBlocks", HaltCooldownBlocks, "txId", tx.Self.TxId)
			return
		}
		kept = append(kept, h)
	}

	dur := tx.Duration
	if dur == 0 {
		dur = HaltDefaultBlocks
	}
	if dur < HaltMinBlocks {
		dur = HaltMinBlocks
	}
	if dur > HaltMaxBlocks {
		dur = HaltMaxBlocks
	}

	kept = append(kept, consensus_state.OutboundHalt{
		Account:      setter,
		Reason:       tx.Reason,
		SetHeight:    bh,
		ExpiryHeight: bh + dur,
		TxId:         tx.Self.TxId,
	})
	if err := se.consensusState.SetOutboundHalts(context.Background(), kept); err != nil {
		log.Warn("vsc.halt: SetOutboundHalts failed", "err", err)
		return
	}
	se.refreshChainConsensusCache()
	log.Info("vsc.halt: gateway outbound path frozen",
		"account", setter, "setHeight", bh, "expiryHeight", bh+dur, "txId", tx.Self.TxId)
}

// executeUnhalt lifts outbound halt(s) EARLY, before auto-expiry — the safety
// valve for a malicious / buggy halt (brief fix 3). Authorized by the recovery
// multisig (M-of-N), never a single node, so stopping stays easy while
// un-stopping stays deliberate. With HaltTxId it lifts only that entry (surgical
// griefer removal, sparing a legitimate solvency halt); empty lifts all active.
// Entries are deactivated (ExpiryHeight <- bh) but RETAINED so the un-halted
// setter's cooldown keeps running (no immediate re-halt).
func (se *StateEngine) executeUnhalt(tx *TxUnhalt) {
	bh := tx.Self.BlockHeight
	if !consensusversion.OutboundHaltActive(se.ActiveConsensusVersion(bh)) {
		return
	}
	if se.consensusState == nil {
		return
	}

	p := se.sconf.ConsensusParams()
	if !VerifyRecoveryMultisig(p, tx.Self.RequiredAuths) {
		// GV-H5: distinguish an unconfigured roster (the early-lift authority is
		// inert — but halts still auto-expire) from a below-threshold attempt.
		if !RecoveryMultisigConfigured(p) {
			log.Warn("vsc.unhalt had NO EFFECT: no recovery multisig is configured for this network — "+
				"the early un-halt authority is INERT (GV-H5). Halts still auto-expire at ExpiryHeight; "+
				"populate RecoveryMultisigAccounts/Threshold to enable early lift.",
				"txId", tx.Self.TxId, "requiredAuths", tx.Self.RequiredAuths)
		} else {
			log.Warn("vsc.unhalt rejected: required_auths do not meet the recovery multisig threshold",
				"txId", tx.Self.TxId, "threshold", p.RecoveryMultisigThreshold, "requiredAuths", tx.Self.RequiredAuths)
		}
		return
	}

	st, err := se.consensusState.Get(context.Background())
	if err != nil {
		log.Warn("vsc.unhalt: consensus state read failed", "err", err)
		return
	}

	changed := false
	next := make([]consensus_state.OutboundHalt, 0, len(st.OutboundHalts))
	for _, h := range st.OutboundHalts {
		if haltCooled(h, bh) {
			continue // fully cooled — prune
		}
		targeted := tx.HaltTxId == "" || h.TxId == tx.HaltTxId
		if targeted && h.ExpiryHeight > bh {
			h.ExpiryHeight = bh // deactivate now; retained for cooldown
			changed = true
		}
		next = append(next, h)
	}
	if !changed {
		log.Warn("vsc.unhalt: no active halt matched", "haltTxId", tx.HaltTxId, "txId", tx.Self.TxId)
	}
	if err := se.consensusState.SetOutboundHalts(context.Background(), next); err != nil {
		log.Warn("vsc.unhalt: SetOutboundHalts failed", "err", err)
		return
	}
	se.refreshChainConsensusCache()
	log.Info("vsc.unhalt: outbound halt(s) lifted early",
		"haltTxId", tx.HaltTxId, "reason", tx.Reason, "atHeight", bh, "txId", tx.Self.TxId)
}
