package tss

import (
	"encoding/base64"
	"math/big"
	"testing"

	"vsc-node/lib/btcvault"
	"vsc-node/modules/db/vsc/elections"
	tss_db "vsc-node/modules/db/vsc/tss"
	"vsc-node/modules/vaultrotation"
)

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
