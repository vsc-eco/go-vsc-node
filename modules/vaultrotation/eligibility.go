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
	// PendingSignerElection maps a PENDING generation's keygen-committee members to
	// the election carrying their BLS keys, so they stay READINESS-eligible for that
	// generation's BRK-2 check-signature even if they churn out of the current
	// election before it activates.
	//
	// DELIBERATELY SEPARATE from SignerElection: that field is also read by the #11
	// bond-lock (state-processing/bond_lock.go), and a Pending generation holds NO
	// funds. Folding Pending into it would bond-lock the very committee this fix
	// exists to keep signing — and because the bond releases only when a generation
	// DRAINS, a generation that never activates would never drain, so those members
	// would be locked permanently. That is strictly worse than the stall being
	// fixed. Consumers must union this in for READINESS only, never for bond-lock.
	PendingSignerElection map[string]elections.ElectionResult
	// PendingKeyIds is the set of PENDING generation keyIds. Kept separate from
	// KeyIds so a pending generation's activation sign can be recognised without
	// granting it the retiring-gen V-A semantics KeyIds carries.
	PendingKeyIds map[string]bool
	// ReshareSkipKeyIds is the set of BTC-vault keyIds the reshare loop must SKIP:
	// every non-active generation — retiring/draining/inactive AND the terminal
	// PURGED. It is deliberately SEPARATE from KeyIds: a purged (fully retired) key
	// must never be reshared (resharing it would resurrect a retired key's shares in
	// the current committee, defeating the fresh-keygen rotation the PR exists for),
	// but the bond-lock / V-A consumers must RELEASE a purged gen's members — so
	// Purged belongs here and NOT in KeyIds. Only the single ACTIVE gen ever reshares.
	ReshareSkipKeyIds map[string]bool
	// Unresolvable is true when "v" was PRESENT but failed to decode (corrupt committed
	// state). The set is then empty and MUST NOT be read as "nobody retiring": the #11
	// bond-lock consumer treats Unresolvable as "everybody locked" (fail-closed), while
	// V-A reads the empty set as a fail-safe freeze. Deterministic — a corrupt "v" is the
	// identical committed blob on every node. (L8-01, FULL-PRUNED.)
	Unresolvable bool
}

// Has reports whether account is a committee member of some fund-holding
// retiring/draining generation.
func (s RetiringSignerSet) Has(account string) bool {
	_, ok := s.SignerElection[account]
	return ok
}

// HasReadiness reports whether account should be treated as READINESS-eligible:
// a member of a fund-holding retiring/draining generation's committee, OR of a
// PENDING generation's keygen committee (so it can still produce that
// generation's BRK-2 check-signature after churning out of the current election).
//
// Use this ONLY for readiness/signing eligibility. Do NOT use it for the #11
// bond-lock: a pending generation holds no funds, and because the bond releases
// only when a generation DRAINS, a generation that never activates would never
// drain and its members would be locked forever. Has() is the bond-lock predicate
// and deliberately excludes Pending.
func (s RetiringSignerSet) HasReadiness(account string) bool {
	if _, ok := s.SignerElection[account]; ok {
		return true
	}
	_, ok := s.PendingSignerElection[account]
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
		SignerElection:        map[string]elections.ElectionResult{},
		PendingSignerElection: map[string]elections.ElectionResult{},
		KeyIds:                map[string]bool{},
		PendingKeyIds:         map[string]bool{},
		ReshareSkipKeyIds:     map[string]bool{},
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
		// L8-01 (FULL-PRUNED): "v" PRESENT but undecodable = corrupt committed state.
		// Fail CLOSED (match output_scoping's resolveVaultView, which refuses all keysigns)
		// by signalling Unresolvable — the #11 bond-lock reads it as everybody-locked
		// rather than releasing bonds on an empty set. Deterministic (same "v" everywhere).
		// The absent-"v" case above stays empty+resolvable (pre-rotation, nobody locked).
		out.Unresolvable = true
		return out
	}
	for i := range vaults {
		v := vaults[i]
		keyId := d.BtcContract + "-" + btcvault.VaultKeyName(v.Generation)

		// Reshare-skip: any NON-ACTIVE generation must be skipped by the reshare loop —
		// retiring/draining/inactive AND the terminal PURGED. This closes the purged-key
		// reshare hole: a purged gen's tss_key stays status:"active" (nothing deactivates it
		// at purge, and KeyRetirementEnabled=false), so without this it would be picked up by
		// FindEpochKeys and RESHARED forever — resurrecting a retired key's shares. Only the
		// single Active gen reshares. Just the keyId string (no committee needed), so it does
		// not depend on the commitment/election reads below.
		switch v.Status {
		case btcvault.VaultStatusPending,
			btcvault.VaultStatusRetiring, btcvault.VaultStatusDraining,
			btcvault.VaultStatusInactive, btcvault.VaultStatusPurged:
			out.ReshareSkipKeyIds[keyId] = true
		}

		// VR2-10: a PENDING generation must not be reshared before it activates.
		// Its tss_key row is already status:"active" the moment keygen commits, so
		// FindEpochKeys selects it, and resharing it collides with the very
		// check-signature that would activate it (VR2-09). It is added to the skip
		// set above; here it contributes its keygen committee to readiness ONLY, so
		// skipping the reshare cannot strand the signers that must produce that
		// signature if the election churns first.
		//
		// Window note: a key committed in epoch E carries Epoch=E, and FindEpochKeys
		// selects strictly `epoch < current`, so it is not reshare-eligible until
		// E+1. Registration (which writes this registry) therefore has a full epoch
		// of head-room; the gap only reopens if registration is slower than an epoch.
		if v.Status == btcvault.VaultStatusPending {
			if elec, accounts, ok := commitmentCommittee(d, keyId); ok {
				for _, account := range accounts {
					out.PendingSignerElection[account] = elec
				}
				out.PendingKeyIds[keyId] = true
			}
			continue
		}

		// L4-C1 (FULL-PRUNED): include INACTIVE, in lock-step with the contract's
		// isFundHoldingStatus / AnyFundedSupersededGen / writeOffDust (all of which count
		// Inactive as fund-holding since S5). Omitting it let a party cheaply re-fund a
		// just-emptied Inactive gen and escape the #11 bond-lock while still holding
		// reconstructable shares. Additive + deterministic; V-A signing of an Inactive gen
		// is independently refused by output_scoping, so this only closes the bond-lock hole.
		if v.Status != btcvault.VaultStatusRetiring && v.Status != btcvault.VaultStatusDraining &&
			v.Status != btcvault.VaultStatusInactive {
			continue
		}
		elec, accounts, ok := commitmentCommittee(d, keyId)
		if !ok {
			continue
		}
		for _, account := range accounts {
			// Any election carrying the member is a valid verification target; if
			// a member sits in more than one retiring gen's committee, LAST-WRITE-
			// WINS in "v" registry iteration order — deterministic across nodes
			// (same committed blob), so arbitrary-but-identical everywhere.
			out.SignerElection[account] = elec
		}
		out.KeyIds[keyId] = true
	}
	return out
}

// commitmentCommittee resolves a keyId's keygen/reshare commitment to the election
// that carries its members' BLS keys, plus the accounts whose commitment bit is
// set. Anchored on the COMMITMENT's own epoch, not the current one, so a
// generation's committee stays resolvable across later election churn.
func commitmentCommittee(d RetiringSignerDeps, keyId string) (elections.ElectionResult, []string, bool) {
	commitment, cerr := d.GetCommitment(keyId)
	if cerr != nil {
		return elections.ElectionResult{}, nil, false
	}
	commitElection := d.GetElection(commitment.Epoch)
	if commitElection == nil || commitElection.Members == nil {
		return elections.ElectionResult{}, nil, false
	}
	bv := new(big.Int)
	if cb, derr := base64.RawURLEncoding.DecodeString(commitment.Commitment); derr == nil {
		bv.SetBytes(cb)
	}
	accounts := make([]string, 0, len(commitElection.Members))
	for midx, member := range commitElection.Members {
		if bv.Bit(midx) == 1 {
			accounts = append(accounts, member.Account)
		}
	}
	return *commitElection, accounts, true
}
