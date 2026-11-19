package btcvault

import (
	"encoding/hex"
	"testing"
)

// A01: the EVM bridge's registry is the BTC layout. This is the entry the
// contract test TestA01_VaultRegistryLayout encodes (account-mapping
// evm-mapping-contract, goldenVaultEntry): generation 2, primary = secp256k1
// key 1, zero backup, Draining, predecessor 1, heights 10/20/30/40. A layout
// drift on either side breaks one of the two tests.
const evmGoldenVaultEntry = "00000002" +
	"0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798" +
	"000000000000000000000000000000000000000000000000000000000000000000" +
	"03" + "00000001" + "0000000a" + "00000014" + "0000001e" + "00000028"

func TestA01_DecodesTheEvmContractsRegistry(t *testing.T) {
	raw, _ := hex.DecodeString(evmGoldenVaultEntry)
	vs, err := UnmarshalVaultRegistry(raw)
	if err != nil || len(vs) != 1 {
		t.Fatalf("decode: %v %v", vs, err)
	}
	v := vs[0]
	if v.Generation != 2 || v.Status != VaultStatusDraining || v.Predecessor != 1 ||
		v.CreatedHeight != 10 || v.ActivatedHeight != 20 || v.RetiredHeight != 30 || v.InactiveHeight != 40 ||
		hex.EncodeToString(v.Primary) != "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798" {
		t.Fatalf("decoded %+v", v)
	}
}

func TestA01_KeyNamesPerBase(t *testing.T) {
	for _, c := range []struct {
		base string
		gen  uint32
		name string
	}{{"main", 0, "main"}, {"main", 3, "mainv3"}, {"primary", 0, "primary"}, {"primary", 12, "primaryv12"}} {
		if got := VaultKeyNameFor(c.base, c.gen); got != c.name {
			t.Fatalf("VaultKeyNameFor(%s,%d) = %s", c.base, c.gen, got)
		}
		if g, ok := VaultGenFromKeyNameFor(c.base, c.name); !ok || g != c.gen {
			t.Fatalf("VaultGenFromKeyNameFor(%s,%s) = %d %v", c.base, c.name, g, ok)
		}
	}
	// Not the other vault's names, and no non-canonical forms.
	for _, bad := range []string{"main", "mainv1", "primaryv0", "primaryv01", "primaryv", "primaryx1"} {
		if _, ok := VaultGenFromKeyNameFor("primary", bad); ok {
			t.Fatalf("%q parsed as an EVM vault key", bad)
		}
	}
	if g, ok := VaultGenFromKeyIdFor("vsc1evm", "primary", "vsc1evm-primaryv4"); !ok || g != 4 {
		t.Fatalf("key id parse: %d %v", g, ok)
	}
	if _, ok := VaultGenFromKeyIdFor("vsc1evm", "primary", "vsc1btc-primaryv4"); ok {
		t.Fatal("another contract's key parsed")
	}
	// The BTC wrappers are unchanged.
	if VaultKeyName(7) != "mainv7" {
		t.Fatal("BTC names changed")
	}
}
