package tss

import (
	"encoding/base64"
	"math/big"
	"strconv"

	"vsc-node/lib/btcvault"
	"vsc-node/modules/db/vsc/elections"
	tss_db "vsc-node/modules/db/vsc/tss"
)

// V-A (BRK-3) — bond-locked retiring-generation signing eligibility.
//
// The migration-sign party is built from the retiring generation's OWN
// keygen/reshare commitment committee (tss.go, `commitElection`), which is
// correct — those members hold the retiring key's shares. But that party is then
// filtered by the gossip readiness set, and readiness is EMITTED only by
// current-election members (the `isMember` gate), VERIFIED on receipt only against
// the current election (p2p.go), and version-floored to the current floor. So once
// >1/3 of the OLD committee churns OUT of the current election (ordinary churn, NO
// attacker), those members can neither emit nor have their readiness accepted →
// they are filtered out → the migration sweep drops below threshold and can NEVER
// sign → the retiring vault stays funded → the next rotation is blocked (NN#3) →
// permanent BTC freeze (C-A / V-A). CSV backup (30d, operator) is the only escape.
//
// The fix widens the CONVERGENT gossip set with an on-chain-DETERMINISTIC addition:
// fund-holding retiring/draining-gen committee members become first-class readiness
// participants for the migration-sign target, regardless of current-election
// membership. This is the OPPOSITE of the reverted GV-H8 mistake — it does NOT
// replace the gossip set with a stale snapshot; each member's readiness stays a
// single BLS-signed, settle-window-converged claim. We only widen WHO may emit /
// be verified, using a set every honest node computes identically from consensus
// state (repo `.claude/CLAUDE.md` Constraint 2 / CHECK 3).

// retiringSignerSet is the deterministic set of extra readiness attesters a
// migration sweep of a fund-holding retiring/draining BTC-vault generation needs.
type retiringSignerSet struct {
	// signerElection maps a retiring-committee member's account to the election
	// that carries its BLS key (the retiring gen's commitment epoch election), so
	// the gossip receive path can verify its attestation.
	signerElection map[string]elections.ElectionResult
	// keyIds is the set of retiring/draining gen keyIds — the sign path uses it to
	// recognise a migration sign and exempt those parties from the CURRENT version
	// floor (V5-6: the OLD key's protocol is fixed at its keygen epoch).
	keyIds map[string]bool
}

func (s retiringSignerSet) has(account string) bool {
	_, ok := s.signerElection[account]
	return ok
}

// retiringSignerDeps are the injectable dependencies of the pure set computation,
// so the deterministic core is unit-testable without a live datalayer / Mongo.
type retiringSignerDeps struct {
	btcContract   string
	readKey       func(key string) ([]byte, bool)                  // contract state at bh
	getCommitment func(keyId string) (tss_db.TssCommitment, error) // keygen/reshare commitment at bh
	getElection   func(epoch uint64) *elections.ElectionResult     // election by epoch
}

// computeRetiringSignerSet is the pure, deterministic core: it reads the BTC vault
// registry, selects fund-holding retiring/draining gens, and unions their
// committee members (from each gen's commitment bitset decoded against its epoch
// election). Every input is consensus state pinned to a single height, so all
// honest nodes compute the identical result (CHECK 3).
func computeRetiringSignerSet(d retiringSignerDeps) retiringSignerSet {
	out := retiringSignerSet{
		signerElection: map[string]elections.ElectionResult{},
		keyIds:         map[string]bool{},
	}
	if d.btcContract == "" {
		return out
	}
	rawV, ok := d.readKey("v")
	if !ok || len(rawV) == 0 {
		return out
	}
	vaults, err := btcvault.UnmarshalVaultRegistry(rawV)
	if err != nil {
		return out
	}
	for i := range vaults {
		v := vaults[i]
		if v.Status != btcvault.VaultStatusRetiring && v.Status != btcvault.VaultStatusDraining {
			continue
		}
		keyId := d.btcContract + "-" + btcvault.VaultKeyName(v.Generation)
		commitment, cerr := d.getCommitment(keyId)
		if cerr != nil {
			continue
		}
		commitElection := d.getElection(commitment.Epoch)
		if commitElection == nil || commitElection.Members == nil {
			continue
		}
		bv := new(big.Int)
		if cb, derr := base64.RawURLEncoding.DecodeString(commitment.Commitment); derr == nil {
			bv.SetBytes(cb)
		}
		for midx, member := range commitElection.Members {
			if bv.Bit(midx) == 1 {
				// Any election carrying the member is a valid verification target;
				// a mismatch (rotated BLS key) only costs a retry (documented on the
				// method below). If a member sits in more than one retiring gen's
				// committee, LAST-WRITE-WINS in "v" registry iteration order — which
				// is deterministic across nodes (same committed blob), so the choice
				// is arbitrary-but-identical everywhere. (Council INFO: not
				// "highest-gen"; do not rely on a gen-ordering guarantee here.)
				out.signerElection[member.Account] = *commitElection
			}
		}
		out.keyIds[keyId] = true
	}
	return out
}

// retiringGenSignerSet computes, at height bh, the committee members of every
// fund-holding RETIRING/DRAINING BTC-vault generation. PURE function of consensus
// state pinned to bh — the BTC vault registry ("v"), each retiring gen's on-chain
// keygen/reshare commitment bitset, and the commitment's epoch election — so every
// honest node computes the IDENTICAL set. Empty (inert) unless
// VaultRotationV2Enabled(bh) AND a retiring/draining BTC gen exists → a
// byte-identical no-op on mainnet today.
//
// Note (devnet-validation edge, degrades SAFE): the member's readiness attestation
// is signed with its CURRENT BLS key; verification here is against the retiring
// gen's commitment epoch election. If a member rotated its BLS key since that epoch,
// verification fails → the member is excluded → the migration retries / falls back
// to the CSV backup path. That is fail-and-retry, never corruption.
func (tssMgr *TssManager) retiringGenSignerSet(bh uint64) retiringSignerSet {
	out := retiringSignerSet{
		signerElection: map[string]elections.ElectionResult{},
		keyIds:         map[string]bool{},
	}
	if tssMgr.sconf == nil || !tssMgr.sconf.ConsensusParams().VaultRotationV2Enabled(bh) {
		return out
	}
	btcContract := tssMgr.sconf.OracleParams().ContractId("BTC")
	if btcContract == "" {
		return out
	}
	return computeRetiringSignerSet(retiringSignerDeps{
		btcContract: btcContract,
		readKey:     tssMgr.contractStateReaderAt(btcContract, bh),
		getCommitment: func(keyId string) (tss_db.TssCommitment, error) {
			return tssMgr.tssCommitments.GetCommitmentByHeight(keyId, bh, "reshare", "keygen")
		},
		getElection: func(epoch uint64) *elections.ElectionResult {
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
func (tssMgr *TssManager) retiringGenSignerSetCached(bh uint64) retiringSignerSet {
	if tssMgr.sconf == nil || !tssMgr.sconf.ConsensusParams().VaultRotationV2Enabled(bh) {
		return retiringSignerSet{
			signerElection: map[string]elections.ElectionResult{},
			keyIds:         map[string]bool{},
		}
	}
	key := strconv.FormatUint(bh, 10)

	tssMgr.retiringSetCacheMu.Lock()
	if s, ok := tssMgr.retiringSetCache[key]; ok {
		tssMgr.retiringSetCacheMu.Unlock()
		return s
	}
	tssMgr.retiringSetCacheMu.Unlock()

	// Compute OUTSIDE the lock — the datalayer read may be slow, and blocking the
	// cache mutex on it would defeat the purpose. A bounded first-miss herd
	// (capped by the pubsub concurrency limit) may each compute once; all
	// subsequent messages for this height hit the cache.
	s := tssMgr.retiringGenSignerSet(bh)

	tssMgr.retiringSetCacheMu.Lock()
	if tssMgr.retiringSetCache == nil {
		tssMgr.retiringSetCache = map[string]retiringSignerSet{}
	}
	// Bound the cache: only a few target heights are ever in flight at once, so
	// clear it wholesale if it somehow grows past a small cap (forces a recompute,
	// never unbounded memory). Cheaper + simpler than per-entry height eviction.
	if len(tssMgr.retiringSetCache) > 64 {
		tssMgr.retiringSetCache = map[string]retiringSignerSet{}
	}
	tssMgr.retiringSetCache[key] = s
	tssMgr.retiringSetCacheMu.Unlock()
	return s
}
