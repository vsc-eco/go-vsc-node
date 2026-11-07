package state_engine

import (
	"testing"

	"vsc-node/lib/btcvault"
	"vsc-node/modules/db/vsc/elections"
	tss_db "vsc-node/modules/db/vsc/tss"
)

// A01: a funded EVM generation locks the bond of every party of every keygen or
// reshare of it, as a BTC generation does (POA-1). Keys are named from the
// "primary" base.
func TestA01_EvmGenerationPartiesLockTheBond(t *testing.T) {
	k := func(gen uint32) string { return "vsc1EVM-" + btcvault.VaultKeyNameFor(btcvault.EvmVaultKeyBase, gen) }
	vaults := []btcvault.Vault{
		{Generation: 0, Status: btcvault.VaultStatusDraining},
		{Generation: 1, Status: btcvault.VaultStatusActive},
		{Generation: 2, Status: btcvault.VaultStatusPending},
		{Generation: 3, Status: btcvault.VaultStatusInactive},
	}
	rows := map[string][]tss_db.TssCommitment{
		k(0): {{Epoch: 1, Commitment: bits(0)}, {Epoch: 2, Commitment: bits(1)}}, // amy (keygen), ben (reshare)
		k(1): {{Epoch: 3, Commitment: bits(2)}},                                  // cat
		k(2): {{Epoch: 3, Commitment: bits(3)}},                                  // dan: pending holds nothing
		k(3): {{Epoch: 3, Commitment: bits(4)}},                                  // eve: inactive released
	}
	elecs := map[uint64][]string{1: {"amy"}, 2: {"amy", "ben"}, 3: {"amy", "ben", "cat", "dan", "eve"}}
	parties, err := fundedVaultPartySet(vaults, "vsc1EVM", btcvault.EvmVaultKeyBase,
		func(keyId string) ([]tss_db.TssCommitment, error) { return rows[keyId], nil },
		func(epoch uint64) (*elections.ElectionResult, error) {
			e := &elections.ElectionResult{}
			for _, a := range elecs[epoch] {
				e.Members = append(e.Members, elections.ElectionMember{Account: a})
			}
			return e, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	for _, who := range []string{"amy", "ben", "cat"} {
		if !parties[who] {
			t.Fatalf("%s holds a share of a funded EVM generation and is not locked", who)
		}
	}
	for _, who := range []string{"dan", "eve"} {
		if parties[who] {
			t.Fatalf("%s locked (pending / inactive generations hold no funds)", who)
		}
	}
	// The BTC names would find none of these keys: the base matters.
	parties, _ = fundedVaultPartySet(vaults, "vsc1EVM", btcvault.BtcVaultKeyBase,
		func(keyId string) ([]tss_db.TssCommitment, error) { return rows[keyId], nil },
		func(uint64) (*elections.ElectionResult, error) { return &elections.ElectionResult{}, nil })
	if len(parties) != 0 {
		t.Fatal("BTC key names matched EVM keys")
	}
}
