// Package vaultrotation holds the deterministic "who is a committee member of a
// fund-holding retiring/draining BTC-vault generation" predicate, shared by the
// two consensus consumers that must agree on it:
//
//   - modules/tss (V-A / BRK-3): those members stay readiness/signing ELIGIBLE for
//     a migration sweep even when churned out of the current election.
//   - modules/state-processing (#11 bond-lock): those members cannot UNSTAKE their
//     consensus bond until their generation is drained.
//
// The two must key off the IDENTICAL set — a member that is signing-eligible but
// bond-releasable (or vice-versa) is an inconsistency. This package is the single
// source of truth. It lives here (not in modules/tss) because modules/tss imports
// modules/state-processing, so state-processing cannot import back into tss; both
// import this leaf instead (it depends only on lib/btcvault + the db model types,
// neither of which imports back — no cycle).
package vaultrotation

import (
	"encoding/base64"
	"math/big"

	"vsc-node/lib/btcvault"
	"vsc-node/modules/db/vsc/elections"
	tss_db "vsc-node/modules/db/vsc/tss"
)

// RetiringSignerSet is the deterministic set of committee members of the
// fund-holding retiring/draining BTC-vault generations at a height.
type RetiringSignerSet struct {
	// SignerElection maps a member's account to the election that carries its BLS
	// key (the retiring gen's commitment epoch election) — used by the tss gossip
	// receive path to verify that member's readiness attestation.
	SignerElection map[string]elections.ElectionResult
	// KeyIds is the set of retiring/draining gen keyIds (used by the tss sign path
	// to recognise a migration sign and exempt those parties from the CURRENT
	// version floor — V5-6).
	KeyIds map[string]bool
}

// Has reports whether account is a committee member of some fund-holding
// retiring/draining generation.
func (s RetiringSignerSet) Has(account string) bool {
	_, ok := s.SignerElection[account]
	return ok
}

// RetiringSignerDeps are the injectable dependencies of the pure computation, so
// the deterministic core is unit-testable without a live datalayer / Mongo and can
// be driven from either consumer's own accessors.
type RetiringSignerDeps struct {
	BtcContract   string
	ReadKey       func(key string) ([]byte, bool)                  // contract state at the height
	GetCommitment func(keyId string) (tss_db.TssCommitment, error) // keygen/reshare commitment at the height
	GetElection   func(epoch uint64) *elections.ElectionResult     // election by epoch
}

// ComputeRetiringSignerSet is the pure, deterministic core: it reads the BTC vault
// registry, selects fund-holding retiring/draining gens, and unions their committee
// members (from each gen's commitment bitset decoded against its epoch election).
// Every input is consensus state pinned to a single height, so all honest nodes
// compute the identical result.
func ComputeRetiringSignerSet(d RetiringSignerDeps) RetiringSignerSet {
	out := RetiringSignerSet{
		SignerElection: map[string]elections.ElectionResult{},
		KeyIds:         map[string]bool{},
	}
	if d.BtcContract == "" {
		return out
	}
	rawV, ok := d.ReadKey("v")
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
		keyId := d.BtcContract + "-" + btcvault.VaultKeyName(v.Generation)
		commitment, cerr := d.GetCommitment(keyId)
		if cerr != nil {
			continue
		}
		commitElection := d.GetElection(commitment.Epoch)
		if commitElection == nil || commitElection.Members == nil {
			continue
		}
		bv := new(big.Int)
		if cb, derr := base64.RawURLEncoding.DecodeString(commitment.Commitment); derr == nil {
			bv.SetBytes(cb)
		}
		for midx, member := range commitElection.Members {
			if bv.Bit(midx) == 1 {
				// Any election carrying the member is a valid verification target; if
				// a member sits in more than one retiring gen's committee, LAST-WRITE-
				// WINS in "v" registry iteration order — deterministic across nodes
				// (same committed blob), so arbitrary-but-identical everywhere.
				out.SignerElection[member.Account] = *commitElection
			}
		}
		out.KeyIds[keyId] = true
	}
	return out
}
