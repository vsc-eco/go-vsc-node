package tss

import (
	"encoding/base64"
	"math/big"
	"testing"

	"vsc-node/lib/btcvault"
	"vsc-node/modules/db/vsc/elections"
	tss_db "vsc-node/modules/db/vsc/tss"
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
	deps := retiringSignerDeps{
		btcContract: scopeContract,
		readKey: func(k string) ([]byte, bool) {
			if k == "v" {
				return reg, true
			}
			return nil, false
		},
		getCommitment: func(keyId string) (tss_db.TssCommitment, error) {
			// gen0's committee = election members at indices 0 and 2.
			return tss_db.TssCommitment{Epoch: 5, Commitment: bitsetB64(0, 2)}, nil
		},
		getElection: func(epoch uint64) *elections.ElectionResult {
			return vaElection(epoch, "alice", "bob", "carol")
		},
	}

	set := computeRetiringSignerSet(deps)
	if !set.has("alice") || !set.has("carol") {
		t.Fatalf("expected alice+carol (bits 0,2) eligible, got %v", set.signerElection)
	}
	if set.has("bob") {
		t.Fatalf("bob (bit 1, not in gen0 committee) must NOT be eligible")
	}
	if !set.keyIds[gen0KeyId] {
		t.Fatalf("expected gen0 keyId %q in the retiring keyId set, got %v", gen0KeyId, set.keyIds)
	}
	// The stored verification election must be the commitment's epoch election.
	if e := set.signerElection["alice"]; e.Epoch != 5 {
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
	if s := computeRetiringSignerSet(retiringSignerDeps{btcContract: "", readKey: readV(nil), getCommitment: commit, getElection: elec}); len(s.signerElection) != 0 {
		t.Fatal("empty contract must yield an empty set")
	}
	// No "v" registry → inert.
	empty := retiringSignerDeps{btcContract: scopeContract, readKey: func(string) ([]byte, bool) { return nil, false }, getCommitment: commit, getElection: elec}
	if s := computeRetiringSignerSet(empty); len(s.signerElection) != 0 {
		t.Fatal("absent registry must yield an empty set")
	}
	// Active-only registry (no retiring/draining gen) → inert.
	activeOnly := marshalRegistry(
		btcvault.Vault{Generation: 0, Primary: scopePub(1), Backup: scopePub(3), Status: btcvault.VaultStatusActive},
	)
	if s := computeRetiringSignerSet(retiringSignerDeps{btcContract: scopeContract, readKey: readV(activeOnly), getCommitment: commit, getElection: elec}); len(s.signerElection) != 0 || len(s.keyIds) != 0 {
		t.Fatal("active-only registry must yield an empty set")
	}
}
