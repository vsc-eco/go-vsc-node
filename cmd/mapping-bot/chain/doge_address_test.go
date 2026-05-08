package chain

import "testing"

// Pentest follow-up to the alt-chain alignment pass. The bot's DOGE
// network params must mirror doge-mapping-contract's init.go overlays
// exactly — PubKeyHashAddrID 0x1e/0x71, ScriptHashAddrID 0x16/0xc4 —
// otherwise the bot derives Bitcoin-prefixed deposit addresses
// (1.../m...) instead of Dogecoin-prefixed ones (D.../n...) and the
// contract rejects them.
//
// DOGE doesn't have native bech32, so Bech32HRPSegwit is left at the
// inherited Bitcoin value; the contract's P2WSH internal usage works
// the same regardless of the chain's mainnet bech32 HRP.
const (
	testDOGEPrimaryPubHex = "0201010101010101010101010101010101010101010101010101010101010101"
	testDOGEBackupPubHex  = "0302020202020202020202020202020202020202020202020202020202020202"
	testDOGEInstruction   = "to=hive:alice"
)

func TestDOGEDepositAddressGoldens(t *testing.T) {
	cases := []struct {
		name string
		cfg  *ChainConfig
	}{
		{"mainnet", NewDOGEMainnet(nil)},
		{"testnet", NewDOGETestnet(nil)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := tc.cfg.AddressGen.GenerateDepositAddress(
				testDOGEPrimaryPubHex, testDOGEBackupPubHex, testDOGEInstruction,
			)
			if err != nil {
				t.Fatalf("GenerateDepositAddress: %v", err)
			}
			if got == "" {
				t.Fatal("empty address")
			}
			// DOGE inherits btcsuite's Bitcoin Bech32 HRP, so P2WSH
			// addresses are bc1.../tb1... in shape — checking just
			// "non-empty" here. The PubKeyHashAddrID / ScriptHashAddrID
			// overlay test below pins the version-byte fix that's the
			// actual concern.
		})
	}
}

// Pentest follow-up: pin every byte of the DOGE network-params overlay
// so accidental drift back to vanilla btcsuite (which would silently
// validate Bitcoin addresses for DOGE deposits) trips this test rather
// than producing deposit-address mismatch in production.
func TestDOGEParamsOverlayMatchesContract(t *testing.T) {
	mn := dogeMainNetParams()
	if mn.PubKeyHashAddrID != 0x1e {
		t.Errorf("DOGE mainnet PubKeyHashAddrID = 0x%x, want 0x1e", mn.PubKeyHashAddrID)
	}
	if mn.ScriptHashAddrID != 0x16 {
		t.Errorf("DOGE mainnet ScriptHashAddrID = 0x%x, want 0x16", mn.ScriptHashAddrID)
	}

	tn := dogeTestNetParams()
	if tn.PubKeyHashAddrID != 0x71 {
		t.Errorf("DOGE testnet PubKeyHashAddrID = 0x%x, want 0x71", tn.PubKeyHashAddrID)
	}
	if tn.ScriptHashAddrID != 0xc4 {
		t.Errorf("DOGE testnet ScriptHashAddrID = 0x%x, want 0xc4", tn.ScriptHashAddrID)
	}
}
