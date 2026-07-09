package state_engine

import (
	"strings"
	datalayer "vsc-node/lib/datalayer"
	"vsc-node/modules/db/vsc/elections"
	tss_db "vsc-node/modules/db/vsc/tss"
	"vsc-node/modules/vaultrotation"

	"github.com/ipfs/go-cid"
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

// btcContractStateReaderAt mirrors the tss-module reader (solvency_gate.go): resolve
// the BTC mapping contract's committed output at height ONCE and return a key-reader
// over its state databin. Always non-nil; it simply MISSES (→ an inert, absent
// registry) if the output can't be resolved, so a bond-lock query fails OPEN
// (unstake allowed) on a resolution error rather than wrongly locking a bond on
// infra trouble. (Same un-timeouted-GetRaw tracked-hardening note as the tss reader;
// contract-state leaves at a processed height are local.)
func (se *StateEngine) btcContractStateReaderAt(contractID string, height uint64) func(key string) ([]byte, bool) {
	miss := func(string) ([]byte, bool) { return nil, false }
	if se.contractState == nil || se.da == nil {
		return miss
	}
	output, err := se.contractState.GetLastOutput(contractID, height)
	if err != nil || output.StateMerkle == "" {
		return miss
	}
	stateCid, err := cid.Parse(output.StateMerkle)
	if err != nil {
		return miss
	}
	databin := datalayer.NewDataBinFromCid(se.da, stateCid)
	return func(key string) ([]byte, bool) {
		keyCid, err := databin.Get(key)
		if err != nil || keyCid == nil {
			return nil, false
		}
		raw, err := se.da.GetRaw(*keyCid)
		if err != nil {
			return nil, false
		}
		return raw, true
	}
}

// IsBondLockedRetiringMember reports whether account is a committee member of a
// fund-holding retiring/draining BTC-vault generation at height (see the package
// doc above). DETERMINISTIC — every input is consensus state pinned to height (the
// BTC vault registry, each retiring gen's keygen/reshare commitment bitset, and the
// commitment's epoch election), fed through the SAME shared predicate as tss V-A, so
// all honest nodes reach the identical verdict for a consensus unstake tx. Empty /
// false (inert) unless VaultRotationV2Enabled(height) AND a retiring/draining BTC
// gen exists → byte-identical no-op on mainnet today.
func (se *StateEngine) IsBondLockedRetiringMember(account string, height uint64) bool {
	if se.sconf == nil || !se.sconf.ConsensusParams().VaultRotationV2Enabled(height) {
		return false
	}
	btcContract := se.sconf.OracleParams().ContractId("BTC")
	if btcContract == "" {
		return false
	}
	set := vaultrotation.ComputeRetiringSignerSet(vaultrotation.RetiringSignerDeps{
		BtcContract: btcContract,
		ReadKey:     se.btcContractStateReaderAt(btcContract, height),
		GetCommitment: func(keyId string) (tss_db.TssCommitment, error) {
			return se.tssCommitments.GetCommitmentByHeight(keyId, height, "reshare", "keygen")
		},
		GetElection: func(epoch uint64) *elections.ElectionResult {
			return se.electionDb.GetElection(epoch)
		},
	})
	return bondLockMatches(set, account)
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
	return set.Has(strings.TrimPrefix(account, "hive:"))
}
