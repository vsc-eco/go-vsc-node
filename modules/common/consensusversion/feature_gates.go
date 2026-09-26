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

// V0_5_0 is the consensus version line at which the v0.5.0 consensus-delegation
// batch activates. Every consensus-affecting change in v0.5.0 keys off this single
// version so the network has ONE coordinated activation, driven by the election
// floor reaching 0.5.0.
var V0_5_0 = Version{Major: 0, Consensus: 5, NonConsensus: 0}

// Version0_5_0Active reports whether the v0.5.0 batch is in force given the
// chain-active consensus version. `active` is resolved by the caller from the
// on-chain election (deterministic, replay-correct). Below the line every v0.5.0
// rule is inert and behavior stays byte-identical to 0.4.0, so old and new
// binaries interoperate until the floor reaches 0.5.0.
func Version0_5_0Active(active Version) bool {
	return active.MeetsConsensusMin(V0_5_0)
}

// DelegatedStakeActive reports whether per-delegator consensus stake/unstake
// semantics are in force given the chain-active consensus version: consensus_stake
// records a per-edge delegation balance (asset "delegation", composite owner
// "from::to") in addition to the unchanged node hive_consensus credit, and
// consensus_unstake authorizes against the SIGNER's edge + debits the node bond,
// so a delegator can always reclaim their own delegated bond and the operator can
// never touch it. Resolve `active` from the version active at the op's block
// height (StateEngine.ActiveConsensusVersion(blockHeight), or the election the tx
// handler already read — see state_engine.DelegatedStakeActiveForElection); below
// the line the legacy hive_consensus-holder unstake path runs byte-identically so
// a full reindex reproduces historical ledger state.
func DelegatedStakeActive(active Version) bool {
	return Version0_5_0Active(active)
}

// V0_7_0 is the consensus version line at which the POA ADMISSION batch
// activates: the seat registry gate on candidacy, the vsc.admit_vote op, flat
// seat-weight, the churn cap, and the collateral exit-halt. Every
// consensus-affecting change in the batch keys off this one version so the
// network has ONE coordinated activation, driven by the election floor reaching
// 0.7.0.
//
// Why 0.7.0 and not 0.4.0: the version line is a FLEET-WIDE namespace, not a
// per-branch one. 0.4.0/0.5.0 are taken by the delegated-consensus-stake batch
// on origin/develop and 0.6.0 by the vault-protection batch on
// origin/feat/vault-protection. Reusing a taken line would mean that, after a
// merge, one floor rise silently activates two unrelated batches at once — the
// exact coordinated-activation property this mechanism exists to provide. 0.7.0
// is the first line free across all three branches (verified by grepping
// V0_[0-9]+_[0-9]+ on each).
var V0_7_0 = Version{Major: 0, Consensus: 7, NonConsensus: 0}

// Version0_7_0Active reports whether the POA admission batch is in force given
// the chain-active consensus version. `active` is resolved by the caller from
// on-chain state (deterministic, replay-correct). Below the line every POA rule
// is inert and behavior stays byte-identical, so old and new binaries
// interoperate until the floor reaches 0.7.0.
func Version0_7_0Active(active Version) bool {
	return active.MeetsConsensusMin(V0_7_0)
}

// PoaAdmissionOpsActive reports whether the POA admission op (vsc.admit_vote) is
// dispatched. Resolve `active` from the version active at the OP's block height
// (StateEngine.ActiveConsensusVersion(blockHeight)), exactly as
// GovernanceActionsActive does for the witness-vote governance trio it is
// modelled on; below the line the op is ignored on every node, so a full reindex
// reproduces historical state.
func PoaAdmissionOpsActive(active Version) bool {
	return Version0_7_0Active(active)
}

// PoaSeatGateActive reports whether election candidacy is restricted to ratified
// POA seats (an allowlist), rather than being open to any sufficiently staked
// witness.
//
// Resolve `active` from the PRIOR ratified election's version — NOT the version
// this election is about to adopt. Two reasons, both load-bearing: it keeps the
// gate out of the version-rise readiness loop (no circular dependency), and it
// gives the network one full epoch after the floor crosses 0.7.0 before the gate
// bites. That epoch of slack is not cosmetic — the structurally identical H-6
// strict-key admission gate starved the mainnet committee below the floor at
// epoch 1699 and halted elections; it is still disabled today (see
// WitnessKeyStrictActive above). A membership gate is the highest-consequence
// change shape in this codebase and is treated accordingly: this gate is also
// inert while the registry is empty, and it runs BEFORE the bond floor guard so
// it can never be the last membership-shrinking step.
func PoaSeatGateActive(active Version) bool {
	return Version0_7_0Active(active)
}

// PoaFlatWeightActive reports whether every ratified seat carries exactly
// params.PoaSeatWeight of election weight, instead of weight tracking the
// account's HIVE_CONSENSUS bond 1:1. Stake keeps its other roles (the MinStake
// eligibility floor, the bond-maturity window, the established-member grace, and
// being the slashable bond) — only its role as consensus WEIGHT is removed, so
// a 2/3 threshold means two-thirds of SEATS rather than two-thirds of stake.
//
// Resolve `active` from the prior ratified election's version, for the same
// determinism reason as PoaSeatGateActive: the weight map is an input to the
// election CID, so every signer regenerating the election must resolve the
// identical verdict or the CIDs diverge and BLS cannot aggregate.
func PoaFlatWeightActive(active Version) bool {
	return Version0_7_0Active(active)
}

// PoaChurnCapActive reports whether the per-election new-member churn cap is in
// force via the POA batch. The pre-existing cap (ConsensusParams
// MaxNewMembersPerElection) is gated on MaxNewMembersActivationHeight, which is
// 0 — inert — on every shipped network, so it is dead code today. Activating it
// off this version gate instead means no future height has to be pinned, which
// removes the documented mis-pin footgun (a bare cap value lets old-binary
// cap=0 and new-binary cap=N nodes compute different member sets for the same
// election). A pinned MaxNewMembersActivationHeight still wins where present.
//
// Resolve `active` from the prior ratified election's version.
func PoaChurnCapActive(active Version) bool {
	return Version0_7_0Active(active)
}

// V0_8_0 is the consensus version line at which the BTC vault-rotation-v2 batch
// activates.
//
// Why 0.8.0: the line is a fleet-wide namespace, not a per-branch one. 0.4.0 and
// 0.5.0 belong to the delegated-consensus-stake batch, 0.6.0 to
// feat/vault-protection, and 0.7.0 to the POA admission batch — so 0.8.0 is the
// first line free across every branch. Reusing a taken line would mean one floor
// rise silently activates two unrelated batches at once, which is exactly the
// coordinated-activation property this mechanism exists to provide.
//
// The ordering is deliberate and not merely numeric: POA (0.7.0) is the staged
// answer to seat-vs-stake weighting, and vault-rotation-v2 follows it.
var V0_8_0 = Version{Major: 0, Consensus: 8, NonConsensus: 0}

// VaultRotationV2Active reports whether the BTC vault-rotation-v2 batch is in
// force given the chain-active consensus version. Below the line every v2 rule is
// inert and behaviour stays byte-identical, so old and new binaries interoperate
// until the floor reaches 0.8.0.
//
// This replaces a BARE ACTIVATION HEIGHT as the coordination mechanism. A height
// pin carries the rolling-upgrade footgun this package exists to remove: every
// witness must be running a binary that carries the pinned height BEFORE the chain
// reaches it, or upgraded and not-yet-upgraded nodes compute different results
// across the gap. The version floor cannot rise until a stake-supermajority
// attests it is RUNNING the code, so a laggard simply fails to drag the floor up
// instead of silently diverging.
//
// Resolve `active` from the version active at the decision point's block height
// (StateEngine.ActiveConsensusVersion(blockHeight)) so a replay recomputes the
// identical verdict.
//
// The explicit ConsensusParams.VaultRotationV2ActivationHeight pin still wins
// where it is set, exactly as PoaChurnCapActive leaves MaxNewMembersActivationHeight
// authoritative. That is what keeps ephemeral networks working: a fresh-genesis
// devnet has no stored election yet, so ActiveConsensusVersion returns 0.0.0 and a
// floor-only gate would be inert at genesis — where the height pin is true from
// block 1. The pin is 0 (disabled) on every shipped network and must stay 0 on
// mainnet, where the attested floor is the only intended path.
func VaultRotationV2Active(active Version) bool {
	return Version0_8_0Active(active)
}

// Version0_8_0Active reports whether the 0.8.0 release batch is in force. Feature
// resolvers on this line delegate here, mirroring Version0_7_0Active, so the line
// is stated once and each call site still reads by FEATURE.
func Version0_8_0Active(active Version) bool {
	return active.MeetsConsensusMin(V0_8_0)
}

// ForcedFloorRespectsQuorumActive reports whether a FORCED version-floor advance
// (ConsensusParams.PinnedVersionFloor / a recovery vsc.propose_consensus_version)
// must still satisfy the H-3/C-2 outgoing-committee quorum guard.
//
// The override was written to bypass BOTH guards, and bypassing them is not the
// same kind of act. The stake-readiness guard measures whether enough of the NEW
// committee's stake has ANNOUNCED the target — a willingness question, and exactly
// the thing an operator should be able to overrule to drag a network past nodes
// that will not upgrade. The H-3/C-2 guard measures whether the OUTGOING committee
// still retains reshare quorum at the target, and that is not a willingness
// question: both signing and resharing are gated on this same floor, so advancing
// past the outgoing committee filters its share-holders below threshold and the
// BTC vault freezes. No later override recovers from that — the shares needed to
// reshare are exactly what the advance filtered out.
//
// So the override keeps its purpose (bypass readiness) and loses the part it could
// never undo. Overruling a guard you cannot recover from is not a recovery lever;
// it is the event you would need to recover FROM.
//
// Version-gated like every other fold here: resolveVersionFloor's output is part
// of the election, so changing which floor a forced proposal produces would alter
// historical elections on a reindex and diverge the CID. Below the line the
// original bypass-both behaviour runs byte-identically.
//
// Resolve `active` from the PRIOR ratified election's version, matching
// BlsWeightDedupActive at the same call site, so the gate stays out of the
// version-rise readiness loop.
func ForcedFloorRespectsQuorumActive(active Version) bool {
	return Version0_8_0Active(active)
}

// BlsWeightDedupActive reports whether committee weight folds collapse duplicate
// BLS keys to a single seat (B13).
//
// A committee holding the same key at two seats produces an aggregate that
// VERIFIES by construction — BlsCircuit walks the keyset by index while
// signatures are keyed by DID, so one honest signature is credited at both
// indices and the aggregate pairs correctly as 2S against 2P. The signature check
// therefore cannot catch it; only the weight fold can, and every fold must do it
// identically or they disagree about whether a block or commitment carries
// quorum.
//
// Version-gated because these folds are consensus accept/reject gates: the
// election-ratification and block-validation folds decide whether a historical
// block or election was VALID, so changing them ungated would flip past verdicts
// during a reindex and fork the chain. Below the line the old fold runs
// byte-identically.
//
// Deliberately NOT WitnessKeyStrictActive. That flag also excludes witnesses
// whose proof-of-possession fails, and enabling it once already starved the
// mainnet committee below the election floor and halted elections (epoch 1699).
// Reusing it would tie this weight fix to that liveness risk and let the PoP
// re-announcement campaign block a fund-safety fix; a separate line keeps the two
// rollouts independent.
//
// Resolve `active` from the PRIOR ratified election's version wherever the result
// feeds an election being built, so the gate stays out of the version-rise
// readiness loop.
func BlsWeightDedupActive(active Version) bool {
	return Version0_8_0Active(active)
}

// TssCommitmentBundleCapActive reports whether an oversized vsc.tss_commitment
// bundle is rejected outright (M-1).
//
// The ingest loop does a staleness check, a DB lookup, a CID hash, a BLS circuit
// deserialisation and a pairing verification PER ELEMENT, off an unauthenticated
// custom_json payload that carried no length check — so one cheap transaction
// could impose all of it on every node.
//
// Version-gated because dropping a transaction's commitments changes indexed
// state: an ungated flip would make a reindex of any historical oversized bundle
// diverge. It is named separately from BlsWeightDedupActive despite resolving to
// the same line, so each call site still reads by FEATURE rather than by batch.
func TssCommitmentBundleCapActive(active Version) bool {
	return Version0_8_0Active(active)
}

// PoaExitHaltActive reports whether the collateral exit-halt binds: a seat's
// consensus bond stays unwithdrawable until PoaExitHaltBlocks after it LEAVES
// the elected set. Resolve `active` from the version active at the height the
// halt is evaluated (StateEngine.ActiveConsensusVersion(height)) so a held or
// released payout recomputes identically on replay.
func PoaExitHaltActive(active Version) bool {
	return Version0_7_0Active(active)
}

// V0_9_0 is the version line of the POA fix batch: corrections to 0.7.0 rules
// found on the live testnet after 0.7.0 had already activated there.
//
// Why a NEW line and not a change to 0.7.0: the testnet has run 0.7.0 with the
// original rules since epoch 1261, so rewriting 0.7.0 would make a node
// replaying that history compute different balances than the chain it joins,
// and a fleet mid-upgrade would split. Below 0.9.0 every 0.7.0 rule keeps its
// original behaviour byte for byte. 0.8.0 is taken by the vault-rotation-v2
// batch; 0.9.0 is free on every branch (grepped V0_[0-9]+_0 across all 87).
// A network that has not reached 0.7.0 (mainnet) can raise its floor straight
// to 0.9.0 and activate both batches together.
var V0_9_0 = Version{Major: 0, Consensus: 9, NonConsensus: 0}

// Version0_9_0Active reports whether the POA fix batch is in force given the
// chain-active consensus version.
func Version0_9_0Active(active Version) bool {
	return active.MeetsConsensusMin(V0_9_0)
}

// PoaHaltOnBondedNodeActive reports whether the submission-time collateral
// exit-halt and retiring-member bond lock also test the account whose bond an
// unstake actually debits. Since the delegated-stake batch (0.5.0) that is the
// NODE (the unstake's `to`), not the signer: testing only the signer let a
// delegator (for example an operator's own alt) pull collateral out from under
// a halted seat (POA-5). The release-time holds are deliberately left alone: the
// bond leaves hive_consensus when the unstake is accepted and a slash only
// reaches hive_consensus, so holding a pending payout protects nothing and would
// only freeze a delegator's already-unbonded funds. Resolve `active` from the
// version active at the transaction's height, exactly like PoaExitHaltActive.
func PoaHaltOnBondedNodeActive(active Version) bool {
	return Version0_9_0Active(active)
}

// PoaAdmissionReopenActive reports whether an EXPIRED admission proposal
// re-opens as a fresh round on the next vote, instead of barring its
// (candidate, owner) pair forever. The proposal id is derived from the pair
// alone, deliberately, so votes converge on one proposal; without a way back
// from "expired", a candidate who missed the window could only ever be admitted
// under a different owner string, and the one-owner-one-seat check is exact on
// that string (POA-7). Resolve `active` from the version active at the VOTE's
// block height, like PoaAdmissionOpsActive.
func PoaAdmissionReopenActive(active Version) bool {
	return Version0_9_0Active(active)
}

// PoaElectorateDropsDepartedActive reports whether a seat stops voting on
// admissions once it has been out of every ratified election for a whole
// departure window (params EffectivePoaVoteDeparture, ten exit-halt windows).
// Below 0.9.0 every seat ever admitted votes forever, so after more than a
// third of seats leave the live set can never reach 2/3 again (POA-3). A seat
// that is only temporarily out of the committee keeps its vote for the window.
// Resolve `active` from the version active at the height the electorate is
// built.
func PoaElectorateDropsDepartedActive(active Version) bool {
	return Version0_9_0Active(active)
}

// PoaStarvationTopUpActive reports how the seat gate is decided. Below 0.9.0
// the gate is judged on the seats among the candidates before the stake and
// version filters: if too few, it is skipped for that epoch and every matured
// staker becomes a candidate at flat seat weight, limited only by the churn cap
// (POA-2); if enough, it applies, and a seat later lost to those filters can
// leave the committee under MinMembers, aborting the election on every retry
// (POA-9). At 0.9.0 the decision is taken after those filters, on the
// survivors: enough seats and the gate applies; too few and every seat is kept
// and only as many unseated candidates as it takes to reach MinMembers are
// admitted, members of the previous committee first, then by matured stake.
// The version-floor readiness ratio is computed over the seats whenever they
// can form the committee. Resolve `active` from the PRIOR ratified election's
// version, like PoaSeatGateActive: the member set is CID input, so every
// signer must reach the identical verdict.
func PoaStarvationTopUpActive(active Version) bool {
	return Version0_9_0Active(active)
}

// PoaBondLockedWhileShareFundedActive reports whether an unstake is refused
// while the bonded account holds a share of a BTC vault generation that still
// holds funds (POA-1). A reshare keeps the key, so every past committee's share
// still combines into a valid signature for as long as that generation is
// funded; the collateral behind such a share stays locked until the generation
// has been rotated out and drained. THORChain's rule: no unbond while a member
// of a vault that still holds funds. Resolve `active` from the version active
// at the unstake's height.
func PoaBondLockedWhileShareFundedActive(active Version) bool {
	return Version0_9_0Active(active)
}

// TssPerAccusedBlameActive reports whether a failed reshare also produces one
// statement per accused party ("reshare_accuse"), each landing on its own 2/3
// BLS quorum, and whether landed statements leave the accused out of the next
// reshare of that key (POA-8). The existing blame commitment is unchanged.
// Resolve `active` from the version active at the session height.
func TssPerAccusedBlameActive(active Version) bool {
	return Version0_9_0Active(active)
}
