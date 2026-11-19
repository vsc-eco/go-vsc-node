package tss

import (
	"strconv"

	"vsc-node/lib/btcvault"

	"vsc-node/modules/common/consensusversion"
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
		SignerElection:        map[string]elections.ElectionResult{},
		PendingSignerElection: map[string]elections.ElectionResult{},
		ActiveSignerElection:  map[string]elections.ElectionResult{},
		KeyIds:                map[string]bool{},
		PendingKeyIds:         map[string]bool{},
		ReshareSkipKeyIds:     map[string]bool{},
	}
}

// retiringGenSignerSet computes, at height bh, the committee members of every
// fund-holding retiring/draining BTC-vault generation (delegates to the shared
// deterministic core). Empty (inert) unless vaultRotationV2InForce(bh) — the
// height pin OR the attested 0.8.0 floor (vault_rotation_gate.go; never the bare
// pin, or the readiness widening would stay inert after a floor-only activation
// and reintroduce the NN#3 stranded-sweep freeze) — AND a retiring/draining BTC
// gen exists → a byte-identical no-op on mainnet today.
//
// Note (devnet-validation edge, degrades SAFE): the member's readiness attestation
// is signed with its CURRENT BLS key; verification (p2p.go) is against the retiring
// gen's commitment epoch election. If a member rotated its BLS key since that
// epoch, verification fails → the member is excluded → the migration retries /
// falls back to the CSV backup. That is fail-and-retry, never corruption.
func (tssMgr *TssManager) retiringGenSignerSet(bh uint64) vaultrotation.RetiringSignerSet {
	if !tssMgr.retiringSetInForce(bh) {
		return emptyRetiringSet()
	}
	btcContract := ""
	if tssMgr.vaultRotationV2InForce(bh) {
		btcContract = tssMgr.sconf.OracleParams().ContractId("BTC")
	}
	// The reader reads the contract's last output as it is built, so build it
	// only for a BTC contract (an EVM-only height never reads "v").
	readKey := func(string) ([]byte, bool) { return nil, false }
	if btcContract != "" {
		readKey = tssMgr.contractStateReaderAt(btcContract, bh)
	}
	evmKeys := tssMgr.evmVaultKeysBound(bh)
	var evmRegs []vaultrotation.RegistrySource
	for _, c := range tssMgr.evmRotatingContracts(bh) {
		evmRegs = append(evmRegs, vaultrotation.RegistrySource{Contract: c, Base: btcvault.EvmVaultKeyBase,
			ReadKey: tssMgr.contractStateReaderAt(c, bh)})
	}
	return vaultrotation.ComputeRetiringSignerSet(vaultrotation.RetiringSignerDeps{
		BtcContract:   btcContract,
		ReadKey:       readKey,
		EvmRegistries: evmRegs,
		GetCommitment: func(keyId string) (tss_db.TssCommitment, error) {
			return tssMgr.tssCommitments.GetCommitmentByHeight(keyId, bh, "reshare", "keygen")
		},
		GetElection: func(epoch uint64) *elections.ElectionResult {
			return tssMgr.electionDb.GetElection(epoch)
		},
		IncludeActiveHolders:   tssMgr.activeKeyHoldersBound(bh),
		EvmVaultKeyIds:         evmKeys,
		IncludeEvmVaultHolders: len(evmKeys) > 0,
	})
}

// retiringSetInForce reports whether retiringGenSignerSet can be non-empty at
// bh: the BTC rules (vault-rotation-v2 in force and a BTC contract) or the EVM
// vault rules (evmVaultKeysBound). Otherwise it is the inert empty set,
// byte-identical to before.
func (tssMgr *TssManager) retiringSetInForce(bh uint64) bool {
	if tssMgr.sconf == nil {
		return false
	}
	if tssMgr.vaultRotationV2InForce(bh) && tssMgr.sconf.OracleParams().ContractId("BTC") != "" {
		return true
	}
	return len(tssMgr.evmVaultKeysBound(bh)) > 0 || len(tssMgr.evmRotatingContracts(bh)) > 0
}

// evmRotatingContracts are the EVM bridge contracts whose vault rolls keys at
// bh (A01, consensusversion.EvmVaultRotationActive on the chain-active
// version): each one's registry feeds the same rules as the BTC registry. Nil
// below 0.13.0.
func (tssMgr *TssManager) evmRotatingContracts(bh uint64) []string {
	if tssMgr.sconf == nil || tssMgr.scheduler == nil {
		return nil
	}
	ids := tssMgr.sconf.OracleParams().EvmBridgeContractIds(bh)
	if len(ids) == 0 || !consensusversion.EvmVaultRotationActive(tssMgr.scheduler.TssMinimumConsensusVersion(bh)) {
		return nil
	}
	return ids
}

// evmRotatingKey reports whether keyId is a generation key of a rotating EVM
// vault at bh ("<contract>-primary" or "-primaryv<N>").
func (tssMgr *TssManager) evmRotatingKey(keyId string, bh uint64) (string, bool) {
	for _, c := range tssMgr.evmRotatingContracts(bh) {
		if _, ok := btcvault.VaultGenFromKeyIdFor(c, btcvault.EvmVaultKeyBase, keyId); ok {
			return c, true
		}
	}
	return "", false
}

// evmVaultKeysBound returns the EVM bridges' vault keys when the 0.13.0
// key-custody rules cover them at bh (consensusversion.EvmVaultCustodyActive
// on the chain-active version), else nil. The key list is per-network config,
// each bridge from its activation height; cheap and local, so it is checked
// before the version read.
func (tssMgr *TssManager) evmVaultKeysBound(bh uint64) []string {
	if tssMgr.sconf == nil || tssMgr.scheduler == nil {
		return nil
	}
	keys := tssMgr.sconf.OracleParams().EvmVaultKeyIds(bh)
	if len(keys) == 0 || !consensusversion.EvmVaultCustodyActive(tssMgr.scheduler.TssMinimumConsensusVersion(bh)) {
		return nil
	}
	return keys
}

// activeKeyHoldersBound reports whether the 0.11.0 active-key-holder rules are in
// force at bh: consensusversion.ActiveKeyHoldersBoundActive on the chain-active
// version, or the vault-rotation-v2 height pin (ephemeral networks only, 0 on
// every shipped network; same inputs as stateEngine.KeyCustodyInForce).
func (tssMgr *TssManager) activeKeyHoldersBound(bh uint64) bool {
	if tssMgr.sconf != nil && tssMgr.sconf.ConsensusParams().VaultRotationV2Enabled(bh) {
		return true
	}
	if tssMgr.scheduler == nil {
		return false
	}
	return consensusversion.ActiveKeyHoldersBoundActive(tssMgr.scheduler.TssMinimumConsensusVersion(bh))
}

// retiringGenSignerSetCached is the memoized accessor used ONLY on the untrusted
// ready_gossip receive path (V-A council A3): that handler runs once per message
// from any peer, and an uncached retiringGenSignerSet does a contract-state
// datalayer read per message, which — sharing the pubsub semaphore with
// consensus-critical TSS messages — a cheap flood could exploit to starve
// signature collection once v2 is live. Caching per target height collapses that
// to at most one read per height regardless of message volume.
//
// It short-circuits (no cache touch, no read) while vaultRotationV2InForce is
// false, so it is byte-identical / allocation-parity with the inert path today.
// The cache is node-local and holds a DETERMINISTIC value (the committed vault
// state at bh), so it never affects consensus; the mutex is never held across the
// datalayer read.
func (tssMgr *TssManager) retiringGenSignerSetCached(bh uint64) vaultrotation.RetiringSignerSet {
	if !tssMgr.retiringSetInForce(bh) {
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
