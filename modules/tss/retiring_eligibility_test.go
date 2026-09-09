package tss

import (
	"encoding/base64"
	"errors"
	"math/big"
	"testing"

	"vsc-node/lib/btcvault"
	"vsc-node/modules/db/vsc/elections"
	tss_db "vsc-node/modules/db/vsc/tss"
	"vsc-node/modules/vaultrotation"
)

// errNoCommitment stands in for a failed commitment lookup.
var errNoCommitment = errors.New("no commitment")

// bitsetB64 encodes a committee bitset (set bits at the given member indices) the
// way the on-chain keygen/reshare commitment stores it.
func bitsetB64(indices ...int) string {
	bv := new(big.Int)
	for _, i := range indices {
		bv.SetBit(bv, i, 1)
	}
	return base64.RawURLEncoding.EncodeToString(bv.Bytes())
}

func vaElection(epoch uint64, accounts ...string) *elections.ElectionResult {
	members := make([]elections.ElectionMember, len(accounts))
	for i, a := range accounts {
		members[i] = elections.ElectionMember{Account: a, Key: "key-" + a}
	}
	return &elections.ElectionResult{
		ElectionCommonInfo: elections.ElectionCommonInfo{Epoch: epoch},
		ElectionDataInfo:   elections.ElectionDataInfo{Members: members},
	}
}

// TestComputeRetiringSignerSet — the V-A deterministic eligibility core: a
// fund-holding retiring/draining gen contributes exactly its on-chain committee
// (bitset ∩ commitment-epoch election); active-only / absent registries contribute
// nothing (inert).
func TestComputeRetiringSignerSet(t *testing.T) {
	gen0KeyId := scopeContract + "-" + btcvault.VaultKeyName(0)
	// gen0 RETIRING (its old committee must stay signing-eligible), gen1 ACTIVE.
	reg := marshalRegistry(
		btcvault.Vault{Generation: 0, Primary: scopePub(1), Backup: scopePub(3), Status: btcvault.VaultStatusRetiring},
		btcvault.Vault{Generation: 1, Primary: scopePub(2), Backup: scopePub(3), Status: btcvault.VaultStatusActive},
	)
	deps := vaultrotation.RetiringSignerDeps{
		BtcContract: scopeContract,
		ReadKey: func(k string) ([]byte, bool) {
			if k == "v" {
				return reg, true
			}
			return nil, false
		},
		GetCommitment: func(keyId string) (tss_db.TssCommitment, error) {
			// gen0's committee = election members at indices 0 and 2.
			return tss_db.TssCommitment{Epoch: 5, Commitment: bitsetB64(0, 2)}, nil
		},
		GetElection: func(epoch uint64) *elections.ElectionResult {
			return vaElection(epoch, "alice", "bob", "carol")
		},
	}

	set := vaultrotation.ComputeRetiringSignerSet(deps)
	if !set.Has("alice") || !set.Has("carol") {
		t.Fatalf("expected alice+carol (bits 0,2) eligible, got %v", set.SignerElection)
	}
	if set.Has("bob") {
		t.Fatalf("bob (bit 1, not in gen0 committee) must NOT be eligible")
	}
	if !set.KeyIds[gen0KeyId] {
		t.Fatalf("expected gen0 keyId %q in the retiring keyId set, got %v", gen0KeyId, set.KeyIds)
	}
	// The stored verification election must be the commitment's epoch election.
	if e := set.SignerElection["alice"]; e.Epoch != 5 {
		t.Fatalf("alice verification election epoch = %d, want 5", e.Epoch)
	}
}

func TestComputeRetiringSignerSet_InertCases(t *testing.T) {
	readV := func(reg []byte) func(string) ([]byte, bool) {
		return func(k string) ([]byte, bool) {
			if k == "v" {
				return reg, true
			}
			return nil, false
		}
	}
	commit := func(string) (tss_db.TssCommitment, error) {
		return tss_db.TssCommitment{Epoch: 5, Commitment: bitsetB64(0)}, nil
	}
	elec := func(epoch uint64) *elections.ElectionResult { return vaElection(epoch, "alice") }

	// Empty BTC contract → inert.
	if s := vaultrotation.ComputeRetiringSignerSet(vaultrotation.RetiringSignerDeps{BtcContract: "", ReadKey: readV(nil), GetCommitment: commit, GetElection: elec}); len(s.SignerElection) != 0 {
		t.Fatal("empty contract must yield an empty set")
	}
	// No "v" registry → inert.
	empty := vaultrotation.RetiringSignerDeps{BtcContract: scopeContract, ReadKey: func(string) ([]byte, bool) { return nil, false }, GetCommitment: commit, GetElection: elec}
	if s := vaultrotation.ComputeRetiringSignerSet(empty); len(s.SignerElection) != 0 {
		t.Fatal("absent registry must yield an empty set")
	}
	// Active-only registry (no retiring/draining gen) → inert.
	activeOnly := marshalRegistry(
		btcvault.Vault{Generation: 0, Primary: scopePub(1), Backup: scopePub(3), Status: btcvault.VaultStatusActive},
	)
	if s := vaultrotation.ComputeRetiringSignerSet(vaultrotation.RetiringSignerDeps{BtcContract: scopeContract, ReadKey: readV(activeOnly), GetCommitment: commit, GetElection: elec}); len(s.SignerElection) != 0 || len(s.KeyIds) != 0 {
		t.Fatal("active-only registry must yield an empty set")
	}
}

// TestComputeRetiringSignerSet_PurgedSkipsReshareButReleasesBond is the regression for
// the purged-key reshare bug: a PURGED (terminal, fully-retired) generation must be in
// ReshareSkipKeyIds (so the reshare loop never reshares its retired key) but NOT in
// KeyIds / SignerElection (so the #11 bond-lock and V-A RELEASE its members — a purged
// gen is drained + past grace, nothing to sign, no reason to stay bond-locked).
func TestComputeRetiringSignerSet_PurgedSkipsReshareButReleasesBond(t *testing.T) {
	gen0KeyId := scopeContract + "-" + btcvault.VaultKeyName(0) // purged
	gen1KeyId := scopeContract + "-" + btcvault.VaultKeyName(1) // retiring
	gen2KeyId := scopeContract + "-" + btcvault.VaultKeyName(2) // active
	reg := marshalRegistry(
		btcvault.Vault{Generation: 0, Primary: scopePub(1), Backup: scopePub(3), Status: btcvault.VaultStatusPurged},
		btcvault.Vault{Generation: 1, Primary: scopePub(2), Backup: scopePub(3), Status: btcvault.VaultStatusRetiring},
		btcvault.Vault{Generation: 2, Primary: scopePub(4), Backup: scopePub(3), Status: btcvault.VaultStatusActive},
	)
	deps := vaultrotation.RetiringSignerDeps{
		BtcContract: scopeContract,
		ReadKey:     func(k string) ([]byte, bool) { return reg, k == "v" },
		GetCommitment: func(string) (tss_db.TssCommitment, error) {
			return tss_db.TssCommitment{Epoch: 5, Commitment: bitsetB64(0)}, nil
		},
		GetElection: func(epoch uint64) *elections.ElectionResult { return vaElection(epoch, "alice") },
	}
	set := vaultrotation.ComputeRetiringSignerSet(deps)

	// Reshare-skip: purged AND retiring are skipped; the ACTIVE gen is NOT (it reshares).
	if !set.ReshareSkipKeyIds[gen0KeyId] {
		t.Errorf("PURGED gen-0 key %q must be in ReshareSkipKeyIds (a retired key must NEVER be reshared)", gen0KeyId)
	}
	if !set.ReshareSkipKeyIds[gen1KeyId] {
		t.Errorf("retiring gen-1 key %q must be in ReshareSkipKeyIds", gen1KeyId)
	}
	if set.ReshareSkipKeyIds[gen2KeyId] {
		t.Errorf("ACTIVE gen-2 key %q must NOT be skipped — the active gen reshares", gen2KeyId)
	}

	// Bond-lock / V-A: the PURGED gen must be released — NOT in KeyIds, its members NOT locked.
	if set.KeyIds[gen0KeyId] {
		t.Errorf("PURGED gen-0 key %q must NOT be in KeyIds — a purged gen's members are released", gen0KeyId)
	}
	if !set.KeyIds[gen1KeyId] {
		t.Errorf("retiring gen-1 key %q must be in KeyIds (still fund-holding)", gen1KeyId)
	}
}

// TestComputeRetiringSignerSet_CorruptRegistryUnresolvable — F14 (vault-v2 failure
// state "corrupt or ambiguous vault registry"). A PRESENT but undecodable "v" must
// come back Unresolvable (the #11 bond-lock reads that as everybody-locked, V-A as a
// freeze), an ABSENT "v" must stay resolvable-and-empty (pre-rotation, nobody locked),
// and the moment "v" decodes again the set is computed normally (the freeze is
// recoverable, no sticky state). Decodable-but-ambiguous registries (0 or 2 Active)
// are NOT unresolvable here: this predicate only ADDS lockers from retiring gens, so
// ambiguity cannot release a bond; the sign-side refusal for those lives in
// output_scoping.resolveVaultView (TestEvaluateScope_UnresolvableRegistryFailsClosed).
func TestComputeRetiringSignerSet_CorruptRegistryUnresolvable(t *testing.T) {
	mk := func(reg []byte, present bool) vaultrotation.RetiringSignerDeps {
		return vaultrotation.RetiringSignerDeps{
			BtcContract: scopeContract,
			ReadKey: func(k string) ([]byte, bool) {
				if k == "v" && present {
					return reg, true
				}
				return nil, false
			},
			GetCommitment: func(keyId string) (tss_db.TssCommitment, error) {
				return tss_db.TssCommitment{Epoch: 5, Commitment: bitsetB64(0, 2)}, nil
			},
			GetElection: func(epoch uint64) *elections.ElectionResult {
				return vaElection(epoch, "alice", "bob", "carol")
			},
		}
	}

	// 1. Corrupt: length is not a multiple of the 91-byte entry size.
	corrupt := make([]byte, btcvault.VaultEntrySize+7)
	set := vaultrotation.ComputeRetiringSignerSet(mk(corrupt, true))
	if !set.Unresolvable {
		t.Fatalf("corrupt 'v' must be Unresolvable (fail closed), got resolvable set %v", set.SignerElection)
	}
	if len(set.SignerElection) != 0 || len(set.KeyIds) != 0 || len(set.ReshareSkipKeyIds) != 0 {
		t.Fatalf("corrupt 'v' must return an EMPTY set alongside Unresolvable, got signers=%v keys=%v skip=%v",
			set.SignerElection, set.KeyIds, set.ReshareSkipKeyIds)
	}

	// 2. Absent: the benign pre-rotation state, resolvable and empty.
	set = vaultrotation.ComputeRetiringSignerSet(mk(nil, false))
	if set.Unresolvable {
		t.Fatal("an ABSENT 'v' must NOT be Unresolvable (pre-rotation: nobody is locked)")
	}
	if len(set.SignerElection) != 0 {
		t.Fatalf("absent 'v' must lock nobody, got %v", set.SignerElection)
	}
	// An empty-but-present blob is the same benign case (contract writes "" before fold).
	set = vaultrotation.ComputeRetiringSignerSet(mk([]byte{}, true))
	if set.Unresolvable {
		t.Fatal("an EMPTY 'v' must NOT be Unresolvable")
	}

	// 3. Recovery: a valid registry again → normal computation, no sticky freeze.
	valid := marshalRegistry(
		btcvault.Vault{Generation: 0, Primary: scopePub(1), Backup: scopePub(3), Status: btcvault.VaultStatusRetiring},
		btcvault.Vault{Generation: 1, Primary: scopePub(2), Backup: scopePub(3), Status: btcvault.VaultStatusActive},
	)
	set = vaultrotation.ComputeRetiringSignerSet(mk(valid, true))
	if set.Unresolvable {
		t.Fatal("a valid 'v' must be resolvable again (the corrupt-registry freeze is recoverable)")
	}
	if !set.Has("alice") || !set.Has("carol") || set.Has("bob") {
		t.Fatalf("valid 'v' after recovery must lock exactly gen-0's committee (alice, carol), got %v", set.SignerElection)
	}

	// 4. Decodable but ambiguous (two Active gens, one Retiring): still resolvable, and
	// the retiring committee stays locked. Ambiguity never RELEASES a bond.
	ambiguous := marshalRegistry(
		btcvault.Vault{Generation: 0, Primary: scopePub(1), Backup: scopePub(3), Status: btcvault.VaultStatusRetiring},
		btcvault.Vault{Generation: 1, Primary: scopePub(2), Backup: scopePub(3), Status: btcvault.VaultStatusActive},
		btcvault.Vault{Generation: 2, Primary: scopePub(4), Backup: scopePub(3), Status: btcvault.VaultStatusActive},
	)
	set = vaultrotation.ComputeRetiringSignerSet(mk(ambiguous, true))
	if set.Unresolvable {
		t.Fatal("a decodable two-Active registry is ambiguous for SIGNING (refused in output_scoping) but must not be Unresolvable here")
	}
	if !set.Has("alice") || !set.Has("carol") {
		t.Fatalf("two-Active registry must still lock gen-0's retiring committee, got %v", set.SignerElection)
	}
	// Duplicate generation entries (Retiring then Active for the same gen): the
	// retiring entry's committee is locked; the duplicate cannot un-lock it.
	dup := marshalRegistry(
		btcvault.Vault{Generation: 0, Primary: scopePub(1), Backup: scopePub(3), Status: btcvault.VaultStatusRetiring},
		btcvault.Vault{Generation: 0, Primary: scopePub(2), Backup: scopePub(3), Status: btcvault.VaultStatusActive},
		btcvault.Vault{Generation: 1, Primary: scopePub(2), Backup: scopePub(3), Status: btcvault.VaultStatusActive},
	)
	set = vaultrotation.ComputeRetiringSignerSet(mk(dup, true))
	if !set.Has("alice") || !set.Has("carol") {
		t.Fatalf("duplicate-generation registry must still lock the retiring entry's committee, got %v", set.SignerElection)
	}
}

// VR2-10 — a PENDING generation must be reshare-SKIPPED and its keygen committee
// must stay READINESS-eligible, WITHOUT ever entering the bond-lock predicate.
//
// Why the separation is load-bearing: the #11 bond-lock calls Has(), which reads
// SignerElection. A pending generation holds NO funds, and the bond releases only
// when a generation DRAINS — so a generation that never activates would never
// drain, and folding Pending into SignerElection would lock its committee
// PERMANENTLY. That is strictly worse than the activation stall being fixed, so
// the readiness widening lives in its own field behind HasReadiness().
func TestComputeRetiringSignerSet_PendingIsReadinessEligibleButNeverBondLocked(t *testing.T) {
	gen1KeyId := scopeContract + "-" + btcvault.VaultKeyName(1)
	// gen0 ACTIVE (the live vault), gen1 PENDING (freshly created, awaiting its
	// BRK-2 check-signature).
	reg := marshalRegistry(
		btcvault.Vault{Generation: 0, Primary: scopePub(1), Backup: scopePub(3), Status: btcvault.VaultStatusActive},
		btcvault.Vault{Generation: 1, Primary: scopePub(2), Backup: scopePub(3), Status: btcvault.VaultStatusPending},
	)
	deps := vaultrotation.RetiringSignerDeps{
		BtcContract: scopeContract,
		ReadKey: func(k string) ([]byte, bool) {
			if k == "v" {
				return reg, true
			}
			return nil, false
		},
		GetCommitment: func(keyId string) (tss_db.TssCommitment, error) {
			// gen1's keygen committee = members at indices 0 and 2.
			return tss_db.TssCommitment{Epoch: 7, Commitment: bitsetB64(0, 2)}, nil
		},
		GetElection: func(epoch uint64) *elections.ElectionResult {
			return vaElection(epoch, "alice", "bob", "carol")
		},
	}

	set := vaultrotation.ComputeRetiringSignerSet(deps)

	// VR2-10: the pending generation must be skipped by the reshare loop. Its
	// tss_key row is already status:"active" the moment keygen commits, so
	// FindEpochKeys would otherwise select it and the reshare would collide with
	// the very check-signature that activates it (VR2-09).
	if !set.ReshareSkipKeyIds[gen1KeyId] {
		t.Fatalf("a PENDING generation must be reshare-skipped; skip set = %v", set.ReshareSkipKeyIds)
	}

	// Readiness: the keygen committee stays eligible so skipping the reshare
	// cannot strand the signers of the activation signature.
	if !set.HasReadiness("alice") || !set.HasReadiness("carol") {
		t.Fatalf("pending gen committee (bits 0,2) must be readiness-eligible; pending set = %v", set.PendingSignerElection)
	}
	if set.HasReadiness("bob") {
		t.Fatal("bob (bit 1) is not in the pending committee and must not be readiness-eligible")
	}
	if !set.PendingKeyIds[gen1KeyId] {
		t.Fatalf("expected the pending keyId in PendingKeyIds, got %v", set.PendingKeyIds)
	}
	// The verification election must be the COMMITMENT's epoch, so the committee
	// stays resolvable after the current election churns.
	if e := set.PendingSignerElection["alice"]; e.Epoch != 7 {
		t.Fatalf("alice pending verification election epoch = %d, want the commitment epoch 7", e.Epoch)
	}

	// THE NEGATIVE CONTROL: the bond-lock predicate must NOT see any of them.
	for _, account := range []string{"alice", "carol"} {
		if set.Has(account) {
			t.Fatalf("%s is a PENDING gen committee member and must NOT be bond-locked: "+
				"Has() feeds the #11 bond-lock, and a never-activated generation never drains, "+
				"so this would lock the bond permanently", account)
		}
	}
	if len(set.SignerElection) != 0 {
		t.Fatalf("a pending-only registry must contribute nothing to SignerElection (the bond-lock field), got %v", set.SignerElection)
	}
	if len(set.KeyIds) != 0 {
		t.Fatalf("a pending generation must not enter KeyIds (retiring-gen V-A semantics), got %v", set.KeyIds)
	}
}

// VR2-10 symmetric fail-mode (found by adversarial review of the fix itself).
//
// The skip and the readiness widening must succeed or fail TOGETHER. Skipping a
// Pending generation's reshare while its committee could not be resolved would
// leave exactly the signers this fix protects with no readiness eligibility —
// the strand scenario the fix exists to prevent. Falling back to "reshareable"
// reinstates only the original, RECOVERABLE collision.
func TestComputeRetiringSignerSet_PendingWithUnresolvableCommitteeStaysReshareable(t *testing.T) {
	gen1KeyId := scopeContract + "-" + btcvault.VaultKeyName(1)
	reg := marshalRegistry(
		btcvault.Vault{Generation: 0, Primary: scopePub(1), Backup: scopePub(3), Status: btcvault.VaultStatusActive},
		btcvault.Vault{Generation: 1, Primary: scopePub(2), Backup: scopePub(3), Status: btcvault.VaultStatusPending},
	)
	deps := vaultrotation.RetiringSignerDeps{
		BtcContract: scopeContract,
		ReadKey: func(k string) ([]byte, bool) {
			if k == "v" {
				return reg, true
			}
			return nil, false
		},
		// The commitment for the pending gen cannot be resolved.
		GetCommitment: func(keyId string) (tss_db.TssCommitment, error) {
			return tss_db.TssCommitment{}, errNoCommitment
		},
		GetElection: func(epoch uint64) *elections.ElectionResult {
			return vaElection(epoch, "alice", "bob", "carol")
		},
	}

	set := vaultrotation.ComputeRetiringSignerSet(deps)

	if set.ReshareSkipKeyIds[gen1KeyId] {
		t.Fatal("a pending generation whose committee is unresolvable must NOT be reshare-skipped: " +
			"skipping without the readiness widening strands its activation signers")
	}
	if len(set.PendingSignerElection) != 0 || set.PendingKeyIds[gen1KeyId] {
		t.Fatal("no readiness widening should have been recorded when the committee is unresolvable")
	}
}
