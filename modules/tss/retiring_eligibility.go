package tss

import (
	"strconv"

	"vsc-node/modules/db/vsc/elections"
	tss_db "vsc-node/modules/db/vsc/tss"
	"vsc-node/modules/vaultrotation"
)

// V-A (BRK-3) — bond-locked retiring-generation signing eligibility.
//
// The migration-sign party is built from the retiring generation's OWN
// keygen/reshare commitment committee (correct — those members hold the retiring
// key's shares). But that party is filtered by the gossip readiness set, and
// readiness is EMITTED only by current-election members (the isMember gate),
// VERIFIED on receipt only against the current election (p2p.go), and
// version-floored. So once >1/3 of the OLD committee churns OUT of the current
// election (ordinary churn, NO attacker), those members can neither emit nor have
// their readiness accepted → filtered out → the migration sweep drops below
// threshold and can NEVER sign → the retiring vault stays funded → the next
// rotation is blocked (NN#3) → permanent BTC freeze (C-A / V-A). CSV backup (30d)
// is the only escape.
//
// The fix widens the CONVERGENT gossip set with an on-chain-DETERMINISTIC addition:
// fund-holding retiring/draining-gen committee members become first-class readiness
// participants for the migration-sign target, regardless of current-election
// membership — the OPPOSITE of the reverted GV-H8 mistake (which replaced the
// gossip set with a stale snapshot). The deterministic set itself lives in the
// shared modules/vaultrotation package so #11 (the consensus bond-lock) keys off
// the IDENTICAL membership.

// emptyRetiringSet is the inert result returned when vault-rotation-v2 is off (or
// no BTC contract) — no allocation surprises for the caller, byte-identical
// behaviour to pre-V-A.
func emptyRetiringSet() vaultrotation.RetiringSignerSet {
	return vaultrotation.RetiringSignerSet{
		SignerElection: map[string]elections.ElectionResult{},
		KeyIds:         map[string]bool{},
	}
}

// retiringGenSignerSet computes, at height bh, the committee members of every
// fund-holding retiring/draining BTC-vault generation (delegates to the shared
// deterministic core). Empty (inert) unless VaultRotationV2Enabled(bh) AND a
// retiring/draining BTC gen exists → a byte-identical no-op on mainnet today.
//
// Note (devnet-validation edge, degrades SAFE): the member's readiness attestation
// is signed with its CURRENT BLS key; verification (p2p.go) is against the retiring
// gen's commitment epoch election. If a member rotated its BLS key since that
// epoch, verification fails → the member is excluded → the migration retries /
// falls back to the CSV backup. That is fail-and-retry, never corruption.
func (tssMgr *TssManager) retiringGenSignerSet(bh uint64) vaultrotation.RetiringSignerSet {
	if tssMgr.sconf == nil || !tssMgr.sconf.ConsensusParams().VaultRotationV2Enabled(bh) {
		return emptyRetiringSet()
	}
	btcContract := tssMgr.sconf.OracleParams().ContractId("BTC")
	if btcContract == "" {
		return emptyRetiringSet()
	}
	return vaultrotation.ComputeRetiringSignerSet(vaultrotation.RetiringSignerDeps{
		BtcContract: btcContract,
		ReadKey:     tssMgr.contractStateReaderAt(btcContract, bh),
		GetCommitment: func(keyId string) (tss_db.TssCommitment, error) {
			return tssMgr.tssCommitments.GetCommitmentByHeight(keyId, bh, "reshare", "keygen")
		},
		GetElection: func(epoch uint64) *elections.ElectionResult {
			return tssMgr.electionDb.GetElection(epoch)
		},
	})
}

// retiringGenSignerSetCached is the memoized accessor used ONLY on the untrusted
// ready_gossip receive path (V-A council A3): that handler runs once per message
// from any peer, and an uncached retiringGenSignerSet does a contract-state
// datalayer read per message, which — sharing the pubsub semaphore with
// consensus-critical TSS messages — a cheap flood could exploit to starve
// signature collection once v2 is live. Caching per target height collapses that
// to at most one read per height regardless of message volume.
//
// It short-circuits (no cache touch, no read) while VaultRotationV2Enabled is
// false, so it is byte-identical / allocation-parity with the inert path today.
// The cache is node-local and holds a DETERMINISTIC value (the committed vault
// state at bh), so it never affects consensus; the mutex is never held across the
// datalayer read.
func (tssMgr *TssManager) retiringGenSignerSetCached(bh uint64) vaultrotation.RetiringSignerSet {
	if tssMgr.sconf == nil || !tssMgr.sconf.ConsensusParams().VaultRotationV2Enabled(bh) {
		return emptyRetiringSet()
	}
	key := strconv.FormatUint(bh, 10)

	tssMgr.retiringSetCacheMu.Lock()
	if s, ok := tssMgr.retiringSetCache[key]; ok {
		tssMgr.retiringSetCacheMu.Unlock()
		return s
	}
	tssMgr.retiringSetCacheMu.Unlock()

	// Compute OUTSIDE the lock — the datalayer read may be slow, and blocking the
	// cache mutex on it would defeat the purpose. A bounded first-miss herd (capped
	// by the pubsub concurrency limit) may each compute once; all subsequent
	// messages for this height hit the cache.
	s := tssMgr.retiringGenSignerSet(bh)

	tssMgr.retiringSetCacheMu.Lock()
	if tssMgr.retiringSetCache == nil {
		tssMgr.retiringSetCache = map[string]vaultrotation.RetiringSignerSet{}
	}
	// Bound the cache: only a few target heights are ever in flight at once, so
	// clear it wholesale if it somehow grows past a small cap (forces a recompute,
	// never unbounded memory).
	if len(tssMgr.retiringSetCache) > 64 {
		tssMgr.retiringSetCache = map[string]vaultrotation.RetiringSignerSet{}
	}
	tssMgr.retiringSetCache[key] = s
	tssMgr.retiringSetCacheMu.Unlock()
	return s
}
