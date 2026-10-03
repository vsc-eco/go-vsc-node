package state_engine

import (
	"errors"
	"fmt"
	"strings"
	datalayer "vsc-node/lib/datalayer"
	"vsc-node/modules/db/vsc/elections"
	tss_db "vsc-node/modules/db/vsc/tss"
	"vsc-node/modules/vaultrotation"

	"github.com/ipfs/go-cid"
	"go.mongodb.org/mongo-driver/mongo"
)

// #11 bond-lock-until-drained.
//
// V-A (modules/tss) keeps a fund-holding retiring/draining BTC-vault generation's
// committee signing-ELIGIBLE even after they churn out of the current election, so
// the migration sweep can still sign. This is the ECONOMIC counterpart: those same
// members must not be able to UNSTAKE their consensus bond until their generation
// is drained — otherwise the freely-churning committee that holds the reconstructable
// old key's shares can bank its keygen reward, unstake, and leave, dropping the
// retiring key below threshold so the migration can never complete (C-A / V-A: a
// permanent BTC freeze with no attacker). Keeping their bond locked gives a rational
// member the incentive to stay online and finish the migration (its bond is released
// only once its generation drains).
//
// The bond-locked set is computed from the IDENTICAL shared vaultrotation predicate
// the tss layer uses for signing eligibility, so a member is signing-eligible IFF it
// is bond-locked — never one without the other.

// isTransientReadErr classifies a consensus-read error for the bond-lock fail-stop.
// A mongo.ErrNoDocuments is a DETERMINISTIC absence — every node sees it identically,
// so concluding "not present" from it cannot diverge/fork — hence it is NOT transient.
// Any OTHER non-nil error is a per-node infra blip that MUST block/retry (else a
// divergent read → divergent unstake outcome → fork). The ErrNoDocuments exclusion is
// load-bearing in BOTH directions: fail to exclude it and blockingRetry loops FOREVER
// on a genuinely-absent generation → a network halt.
func isTransientReadErr(err error) bool {
	return err != nil && !errors.Is(err, mongo.ErrNoDocuments)
}

// btcContractStateReaderAtStrict is the FAIL-STOP contract-state reader for the #11
// bond-lock CONSENSUS path (council F2). It resolves the BTC mapping contract's
// committed output at height and returns a key-reader over its state databin, but —
// unlike the tss-side reader, which feeds a LOCAL keysign decision that tolerates
// divergence via fail-and-retry — it distinguishes a TRANSIENT per-node Mongo error
// (sets *transient so the caller blocks/retries → all nodes converge, no fork) from a
// DETERMINISTIC condition that is identical on every node (genuine absence, or a
// parse/decode error of committed content-addressed state), which is an inert miss
// (fail-open, all nodes agree — safe, no divergence). The datalayer reads
// (databin/GetRaw) BLOCK-and-fetch via the online bitswap blockservice, so a
// non-local leaf never silently fails open — it blocks (network-wide unavailability
// stalls processing, never forks); an actual datalayer error is a deterministic
// corruption → inert miss.
func (se *StateEngine) btcContractStateReaderAtStrict(contractID string, height uint64, transient *error) func(key string) ([]byte, bool) {
	miss := func(string) ([]byte, bool) { return nil, false }
	if se.contractState == nil || se.da == nil {
		return miss
	}
	output, err := se.contractState.GetLastOutputStrict(contractID, height)
	if err != nil {
		if isTransientReadErr(err) {
			*transient = err // per-node Mongo blip → the caller blocks/retries
		}
		return miss // ErrNoDocuments = deterministic absence; a transient result is discarded + retried
	}
	if output.StateMerkle == "" {
		return miss // deterministic: the contract has no state yet
	}
	stateCid, perr := cid.Parse(output.StateMerkle)
	if perr != nil {
		return miss // committed StateMerkle is identical on all nodes → deterministic, fail-open
	}
	databin := datalayer.NewDataBinFromCid(se.da, stateCid)
	return func(key string) ([]byte, bool) {
		keyCid, gerr := databin.Get(key)
		if gerr != nil || keyCid == nil {
			return nil, false // datalayer blocks for non-local; an error here is deterministic corruption
		}
		raw, rerr := se.da.GetRaw(*keyCid)
		if rerr != nil {
			return nil, false // deterministic
		}
		return raw, true
	}
}

// IsBondLockedRetiringMember reports whether account is a committee member of a
// fund-holding retiring/draining BTC-vault generation at height (see the package
// doc). It drives a CONSENSUS tx outcome (TxConsensusUnstake success/reject + the
// ledger unstake mutation), so it is FAIL-STOP (council F2): a per-node transient
// read error must NOT fail-open (→ a divergent unstake result → fork). blockingRetry
// re-reads until every underlying read either SUCCEEDS or shows a genuine
// (all-nodes-identical) absence — only then is the verdict committed; a persistent
// infra failure stalls the slot network-wide (recoverable), never forks. Fed through
// the SAME shared predicate as tss V-A (signing-eligible IFF bond-locked). Empty /
// false (inert) unless VaultRotationV2Enabled(height) AND a retiring/draining BTC gen
// exists → byte-identical no-op on mainnet today.
func (se *StateEngine) IsBondLockedRetiringMember(account string, height uint64) bool {
	if se.sconf == nil || !se.sconf.ConsensusParams().VaultRotationV2Enabled(height) {
		return false
	}
	btcContract := se.sconf.OracleParams().ContractId("BTC")
	if btcContract == "" {
		return false
	}
	var locked bool
	blockingRetry(fmt.Sprintf("bondLock(%s,%d)", account, height), func() error {
		var transient error
		reader := se.btcContractStateReaderAtStrict(btcContract, height, &transient)
		set := vaultrotation.ComputeRetiringSignerSet(vaultrotation.RetiringSignerDeps{
			BtcContract: btcContract,
			ReadKey:     reader,
			GetCommitment: func(keyId string) (tss_db.TssCommitment, error) {
				c, cerr := se.tssCommitments.GetCommitmentByHeight(keyId, height, "reshare", "keygen")
				if isTransientReadErr(cerr) {
					transient = cerr // per-node blip -> block/retry
				}
				return c, cerr // ErrNoDocuments (deterministic absence) → the predicate skips this gen
			},
			GetElection: func(epoch uint64) *elections.ElectionResult {
				el, eerr := se.electionDb.GetElectionStrict(epoch)
				if isTransientReadErr(eerr) {
					transient = eerr // per-node blip -> block/retry
				}
				return el // nil (deterministic absence) → the predicate skips this gen
			},
		})
		if transient != nil {
			return transient // block/retry until infra recovers; the set is discarded
		}
		locked = bondLockMatches(set, account)
		return nil
	})
	return locked
}

// bondLockMatches applies the account-namespace normalization required by the
// consensus-unstake call site (#11 council F1). A consensus_unstake carries
// tx.From in "hive:<account>" form (the handler rejects anything else), but the
// retiring-committee set is keyed by BARE election account names
// (elections.ElectionMember.Account, e.g. "alice"). Without stripping the prefix
// the map lookup can never hit and the whole gate is dead code (returns false for
// every real witness). The tss/V-A consumer is unaffected — it compares the bare
// HiveUsername to bare keys — so only this Hive-namespaced consumer must normalize.
func bondLockMatches(set vaultrotation.RetiringSignerSet, account string) bool {
	// L8-01 (FULL-PRUNED): fail CLOSED on a corrupt-"v" (Unresolvable) registry — treat
	// EVERY member as bond-locked rather than releasing a bond we cannot verify, so a
	// corrupt-registry window can't let a member holding reconstructable shares unstake.
	// Deterministic (a corrupt "v" is the identical committed blob on every node) → no fork.
	if set.Unresolvable {
		return true
	}
	return set.Has(strings.TrimPrefix(account, "hive:"))
}
