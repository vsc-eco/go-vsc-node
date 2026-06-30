package consensusversion

// feature_gates.go houses the "is feature X active?" resolvers for consensus
// features whose rollout is coordinated by the CHAIN-ACTIVE CONSENSUS VERSION
// (the election version floor) rather than a fixed L1 height.
//
// Each resolver takes the ALREADY-RESOLVED chain-active version at the relevant
// decision point and answers a single feature question. The caller resolves
// that version deterministically from on-chain state — e.g.
// StateEngine.ActiveConsensusVersion(blockHeight) (== ResultVersion of the
// election active at that height), or, for an election being built, the prior
// ratified election's version — so every node and signer reaches the identical
// verdict (Constraint 3: identical committee / state / CID).
//
// Why version-gated (not height-gated): the floor can only rise to a target
// once a stake-supermajority of witnesses attests it is RUNNING that version
// (see election-proposer GenerateFullElection + ConsensusVersionActivation*).
// So a feature cannot activate before the network is actually running code that
// implements it — this removes the height gate's deploy footgun ("every witness
// must run a binary carrying the height BEFORE the chain reaches it, or upgraded
// and not-yet-upgraded nodes diverge across the gap"). A laggard simply does not
// drag the floor up, rather than silently forking mid-gap.
//
// Per-network rollout is therefore expressed through the consensus-version floor
// (ConsensusParams.ConsensusVersionFloor{Epoch,Major,Consensus} / a
// vsc.propose_consensus_version), NOT a per-feature activation height.

// V0_2_0 is the consensus version line at which the v0.2.0 release batch
// activates. Every consensus-affecting change shipping in v0.2.0 keys off this
// single version so the network has ONE coordinated activation, driven by the
// election floor reaching 0.2.0. try/catch ICC (TryCatchICCVersion) and the
// pendulum LP-floor (incentive-pendulum LPFloorActivation) gate on this same
// line; the resolvers below are the rest of the batch.
var V0_2_0 = Version{Major: 0, Consensus: 2, NonConsensus: 0}

// Version0_2_0Active reports whether the v0.2.0 release batch is in force given
// the chain-active consensus version. `active` is resolved by the caller from
// the on-chain election (deterministic, replay-correct). Below the line every
// v0.2.0 rule is inert and behavior stays byte-identical to 0.1.0, so old and
// new binaries interoperate until the floor reaches 0.2.0.
func Version0_2_0Active(active Version) bool {
	return active.MeetsConsensusMin(V0_2_0)
}

// WitnessKeyStrictActive reports whether the election build should strictly
// enforce consensus + gateway key admission (audit H-6): exclude any witness
// whose consensus BLS key or gateway secp256k1 key fails its proof-of-possession,
// and dedupe the committee by each key (keeping the account-lexicographically-
// first witness on a collision).
//
// Resolve `active` from the PRIOR ratified election's version (the chain-active
// version at the election anchor), NOT the version this election is about to
// adopt — that keeps the gate out of the version-rise readiness loop (no
// circular dependency) and gives the network one full epoch after the floor
// crosses 0.2.0 for witnesses to (re-)announce a valid PoP before the gate bites,
// which is exactly the safety the prior dedicated height was hand-positioned to
// provide.
func WitnessKeyStrictActive(active Version) bool {
	// TEMPORARILY DISABLED (2026-06-22) — emergency liveness fix. The H-6 strict
	// PoP gate starved the mainnet committee below the floor at epoch 1699,
	// halting elections (1698 was the last election produced). Returning false
	// reverts to the pre-0.2.0 warn-only key behavior so the committee re-fills
	// and elections resume. RE-ENABLE (restore the line below) once witnesses
	// have re-announced valid consensus + gateway-key PoPs.
	return false
	// return Version0_2_0Active(active)
}

// ContractUpdateTimelockActive reports whether the contract-update timelock (and
// its cancel_contract_update op) is in force given the chain-active consensus
// version. Resolve `active` from the version active at the update's submit
// height (StateEngine.ActiveConsensusVersion(submitHeight)); below the line
// updates stay immediate so a full reindex reproduces historical state.
func ContractUpdateTimelockActive(active Version) bool {
	return Version0_2_0Active(active)
}

// GatewayDecentralizationActive reports whether a gateway key rotation should
// REMOVE the vsc.dao owner-authority backstop (audit A3-2), given the chain-
// active consensus version. Resolve `active` from the version active at the
// rotation height (ResultVersion of the election at that height).
func GatewayDecentralizationActive(active Version) bool {
	return Version0_2_0Active(active)
}

// V0_3_0 is the consensus version line at which the v0.3.0 witness-vote
// GOVERNANCE batch activates. Every consensus-affecting change in v0.3.0 keys off
// this single version so the network has ONE coordinated activation, driven by
// the election floor reaching 0.3.0.
var V0_3_0 = Version{Major: 0, Consensus: 3, NonConsensus: 0}

// Version0_3_0Active reports whether the v0.3.0 batch is in force given the
// chain-active consensus version. `active` is resolved by the caller from the
// on-chain election (deterministic, replay-correct). Below the line every v0.3.0
// rule is inert and behavior stays byte-identical to 0.2.0, so old and new
// binaries interoperate until the floor reaches 0.3.0.
func Version0_3_0Active(active Version) bool {
	return active.MeetsConsensusMin(V0_3_0)
}

// GovernanceActionsActive reports whether the witness-vote governance ops
// (vsc.slash_restore, vsc.reserve_payout, vsc.reserve_vote) are in force given
// the chain-active consensus version. Resolve `active` from the version active at
// the op's block height (StateEngine.ActiveConsensusVersion(blockHeight)); below
// the line the ops are ignored on every node, so a full reindex reproduces
// historical state and a laggard is excluded from the committee rather than
// applying the ops over a chain that didn't.
func GovernanceActionsActive(active Version) bool {
	return Version0_3_0Active(active)
}

// SafetySlashBurnDelay7dActive reports whether a new safety slash uses the
// extended 7-day pending-burn window (vs the original 3 days), given the chain-
// active consensus version. Resolve `active` from the version active at the
// SLASH's own height (StateEngine.ActiveConsensusVersion(slashHeight)) so the
// stored maturity (slashHeight + delay) recomputes identically on replay; below
// the line the window stays 3 days. It shares the v0.3.0 line with the governance
// ops it backs, so both flip together.
func SafetySlashBurnDelay7dActive(active Version) bool {
	return Version0_3_0Active(active)
}

// V0_4_0 is the consensus version line at which the v0.4.0 safety/correctness
// batch activates. Every consensus-affecting change in v0.4.0 keys off this single
// version so the network has ONE coordinated activation, driven by the election
// floor reaching 0.4.0.
var V0_4_0 = Version{Major: 0, Consensus: 4, NonConsensus: 0}

// Version0_4_0Active reports whether the v0.4.0 batch is in force given the
// chain-active consensus version. `active` is resolved by the caller from the
// on-chain election (deterministic, replay-correct). Below the line every v0.4.0
// rule is inert and behavior stays byte-identical to 0.3.0, so old and new
// binaries interoperate until the floor reaches 0.4.0.
func Version0_4_0Active(active Version) bool {
	return active.MeetsConsensusMin(V0_4_0)
}

// MinMembersGuardActive reports whether the GV4-3 sub-MinMembers election-reject
// guard is enforced given the chain-active consensus version: the state engine
// rejects an incoming election whose new committee has < MinMembers members before
// persisting it (see TxElectionResult.ExecuteTx), so a degenerate committee can
// never drive consensus.GenerateSchedule into witnessList[slot % 0] (a divide-by-
// zero chain halt). Resolve `active` from the version active at the election's
// submit height (StateEngine.ActiveConsensusVersion(tx.Self.BlockHeight)); below
// the line the reject is inert so replay of pre-activation history (which on
// mainnet includes valid 7-member elections) is byte-identical. A sub-MinMembers
// election is never legitimate, so the guard can only ever reject degenerate input.
func MinMembersGuardActive(active Version) bool {
	return Version0_4_0Active(active)
}

// UnstakeHbdDirectionFixActive reports whether the F14 fix is in force given the
// chain-active consensus version: an offchain unstake_hbd op builds TxUnstakeHbd
// (releases stake) instead of the legacy TxStakeHbd (which wrongly STAKED — the
// 0.3.0 behavior). Resolve `active` from the version active at the op's anchored
// height (StateEngine.ActiveConsensusVersion(anchoredHeight)); below the line the
// legacy stake direction is preserved so a full reindex reproduces historical
// ledger state. The L1 vsc.unstake_hbd path was already correct and is unaffected.
func UnstakeHbdDirectionFixActive(active Version) bool {
	return Version0_4_0Active(active)
}
