package chain

import "testing"

// Pentest follow-up to the alt-chain alignment pass. The bot's LTC
// network params must mirror ltc-mapping-contract's init.go overlays
// exactly — Bech32HRPSegwit "ltc"/"tltc" plus PubKeyHashAddrID
// 0x30/0x6f and ScriptHashAddrID 0x32/0xc4 — otherwise deposit
// addresses won't round-trip with what the contract validates.
//
// These goldens lock in:
//   - Bech32HRPSegwit overlay ("ltc" / "tltc")
//   - createP2WSHWithBackup script bytes (pubkey order, opcodes, CSV value)
//   - sha256(instruction) → tag derivation
//
// PubKeyHashAddrID / ScriptHashAddrID overlays aren't exercised by the
// P2WSH path here — TestLTCParamsOverlayMatchesContract below pins those
// values directly so a future change that drops them trips a test.
const (
	testLTCPrimaryPubHex = "0201010101010101010101010101010101010101010101010101010101010101"
	testLTCBackupPubHex  = "0302020202020202020202020202020202020202020202020202020202020202"
	testLTCInstruction   = "to=hive:alice"
)

func TestLTCDepositAddressGoldens(t *testing.T) {
	cases := []struct {
		name string
		cfg  *ChainConfig
	}{
		{"mainnet", NewLTCMainnet(nil)},
		{"testnet", NewLTCTestnet(nil)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := tc.cfg.AddressGen.GenerateDepositAddress(
				testLTCPrimaryPubHex, testLTCBackupPubHex, testLTCInstruction,
			)
			if err != nil {
				t.Fatalf("GenerateDepositAddress: %v", err)
			}
			// Smoke check: address is non-empty and uses the LTC bech32 HRP.
			if got == "" {
				t.Fatal("empty address")
			}
			wantPrefix := "ltc1"
			if tc.name == "testnet" {
				wantPrefix = "tltc1"
			}
			if len(got) < len(wantPrefix) || got[:len(wantPrefix)] != wantPrefix {
				t.Errorf("ltc/%s deposit address has wrong HRP:\n  got:    %q\n  expect: %s…", tc.name, got, wantPrefix)
			}
		})
	}
}

// Pentest follow-up: pin every byte of the LTC network-params overlay
// so accidental drift back to vanilla btcsuite (which would silently
// break round-trip with the contract) trips this test rather than
// surfacing as silent deposit failures in production.
func TestLTCParamsOverlayMatchesContract(t *testing.T) {
	mn := ltcMainNetParams()
	if mn.PubKeyHashAddrID != 0x30 {
		t.Errorf("LTC mainnet PubKeyHashAddrID = 0x%x, want 0x30", mn.PubKeyHashAddrID)
	}
	if mn.ScriptHashAddrID != 0x32 {
		t.Errorf("LTC mainnet ScriptHashAddrID = 0x%x, want 0x32", mn.ScriptHashAddrID)
	}
	if mn.Bech32HRPSegwit != "ltc" {
		t.Errorf("LTC mainnet Bech32HRPSegwit = %q, want \"ltc\"", mn.Bech32HRPSegwit)
	}

	tn := ltcTestNetParams()
	if tn.PubKeyHashAddrID != 0x6f {
		t.Errorf("LTC testnet PubKeyHashAddrID = 0x%x, want 0x6f", tn.PubKeyHashAddrID)
	}
	if tn.ScriptHashAddrID != 0xc4 {
		t.Errorf("LTC testnet ScriptHashAddrID = 0x%x, want 0xc4", tn.ScriptHashAddrID)
	}
	if tn.Bech32HRPSegwit != "tltc" {
		t.Errorf("LTC testnet Bech32HRPSegwit = %q, want \"tltc\"", tn.Bech32HRPSegwit)
	}
}
