package tss

import (
	"encoding/binary"
	"errors"
	"testing"

	"vsc-node/lib/btcvault"
	"vsc-node/modules/db/vsc/elections"
	tss_db "vsc-node/modules/db/vsc/tss"
	"vsc-node/modules/vaultrotation"
)

// A01: an EVM bridge's vault registry drives the shared signer-set core
// exactly as the BTC registry does, with its own key names.
func TestA01_SignerSetFromAnEvmRegistry(t *testing.T) {
	const c = "vsc1EvmBridge"
	k := func(gen uint32) string { return c + "-" + btcvault.VaultKeyNameFor(btcvault.EvmVaultKeyBase, gen) }
	entry := func(gen uint32, st btcvault.VaultStatus) []byte {
		e := make([]byte, btcvault.VaultEntrySize)
		binary.BigEndian.PutUint32(e, gen)
		e[70] = byte(st)
		return e
	}
	var reg []byte
	reg = append(reg, entry(0, btcvault.VaultStatusDraining)...)
	reg = append(reg, entry(1, btcvault.VaultStatusActive)...)
	reg = append(reg, entry(2, btcvault.VaultStatusPending)...)
	reg = append(reg, entry(3, btcvault.VaultStatusPurged)...)
	commits := map[string]tss_db.TssCommitment{
		k(0): {Epoch: 9, Commitment: bitsetB64(2, 3)}, // carol, dave
		k(1): {Epoch: 9, Commitment: bitsetB64(0, 1)}, // alice, bob
		k(2): {Epoch: 9, Commitment: bitsetB64(4)},    // erin
	}
	elec := vaElection(9, "alice", "bob", "carol", "dave", "erin")
	deps := func(raw []byte) vaultrotation.RetiringSignerDeps {
		return vaultrotation.RetiringSignerDeps{
			GetCommitment: func(id string) (tss_db.TssCommitment, error) {
				if r, ok := commits[id]; ok {
					return r, nil
				}
				return tss_db.TssCommitment{}, errors.New("none")
			},
			GetElection:            func(uint64) *elections.ElectionResult { return elec },
			EvmVaultKeyIds:         []string{c + "-primary"},
			IncludeEvmVaultHolders: true,
			EvmRegistries: []vaultrotation.RegistrySource{{Contract: c, Base: btcvault.EvmVaultKeyBase,
				ReadKey: func(key string) ([]byte, bool) { return raw, key == "v" && raw != nil }}},
		}
	}

	s := vaultrotation.ComputeRetiringSignerSet(deps(reg))
	for _, gen := range []uint32{0, 2, 3} {
		if !s.ReshareSkipKeyIds[k(gen)] {
			t.Fatalf("gen %d not reshare-skipped", gen)
		}
	}
	if s.ReshareSkipKeyIds[k(1)] {
		t.Fatal("the active generation is reshare-skipped")
	}
	if !s.KeyIds[k(0)] || !s.Has("carol") || !s.Has("dave") || s.Has("alice") {
		t.Fatalf("retiring committee: keys %v signers %v", s.KeyIds, s.SignerElection)
	}
	if _, ok := s.ActiveSignerElection["alice"]; !ok {
		t.Fatalf("active holders: %v", s.ActiveSignerElection)
	}
	// C1 is replaced by the registry: "-primary" is now the retiring generation 0,
	// whose committee must not ride in as active holders.
	if _, ok := s.ActiveSignerElection["carol"]; ok {
		t.Fatal("the C1 newest-holders path still ran for a registered bridge")
	}
	if !s.PendingKeyIds[k(2)] || !s.HasReadiness("erin") || s.Has("erin") {
		t.Fatal("pending generation: readiness only")
	}

	// No registry yet: C1 holds the bridge's one key.
	s = vaultrotation.ComputeRetiringSignerSet(deps(nil))
	if _, ok := s.ActiveSignerElection["carol"]; !ok || len(s.ReshareSkipKeyIds) != 0 {
		t.Fatalf("pre-registry bridge: active %v skip %v", s.ActiveSignerElection, s.ReshareSkipKeyIds)
	}

	// A corrupt registry fails closed (bond lock: everyone locked).
	if s := vaultrotation.ComputeRetiringSignerSet(deps([]byte{1, 2, 3})); !s.Unresolvable {
		t.Fatal("corrupt EVM registry is resolvable")
	}
}
