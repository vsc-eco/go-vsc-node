package chain

import "testing"

// Goldens: bot's Dash deposit-address output for fixed inputs.
//
// The recorded values are what BTCAddressGenerator.GenerateDepositAddress
// produces given the params from NewDASHMainnet / NewDASHTestnet /
// NewDASHRegtest. They lock in:
//   - Bech32HRPSegwit overlay ("dash" / "tdash" / "bcrt")
//   - createP2WSHWithBackup script bytes (pubkey order, opcodes, CSV value)
//   - sha256(instruction) → tag derivation
//
// If you intentionally change any of the above, regenerate by running this
// test, copying the actual values from the failure output, and re-running.
//
// These goldens cover the HRP path. PubKeyHashAddrID / ScriptHashAddrID
// overlays are not exercised here because the bot's deposit addresses are
// always P2WSH.
const (
	testDashPrimaryPubHex = "0201010101010101010101010101010101010101010101010101010101010101"
	testDashBackupPubHex  = "0302020202020202020202020202020202020202020202020202020202020202"
	testDashInstruction   = "to=hive:alice"

	wantDashMainnetAddr = "dash1q6335nmxfaevm2exne774unngsr7pn0jx7hfm5ux7v9xkwx0r40mqrnu2e7"
	wantDashTestnetAddr = "tdash1q9az0hh9pnnmdjuvpgkpqm4nu647qsxcyg7t7n8cnzkgtfhuaft6s400yc0"
	wantDashRegtestAddr = "bcrt1q9az0hh9pnnmdjuvpgkpqm4nu647qsxcyg7t7n8cnzkgtfhuaft6swcqhug"
)

func TestDashDepositAddressGoldens(t *testing.T) {
	cases := []struct {
		name string
		cfg  *ChainConfig
		want string
	}{
		{"mainnet", NewDASHMainnet(nil), wantDashMainnetAddr},
		{"testnet", NewDASHTestnet(nil), wantDashTestnetAddr},
		{"regtest", NewDASHRegtest(nil), wantDashRegtestAddr},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := tc.cfg.AddressGen.GenerateDepositAddress(
				testDashPrimaryPubHex, testDashBackupPubHex, testDashInstruction,
			)
			if err != nil {
				t.Fatalf("GenerateDepositAddress: %v", err)
			}
			if got != tc.want {
				t.Errorf("dash/%s deposit address mismatch:\n  got:  %q\n  want: %q",
					tc.name, got, tc.want)
			}
		})
	}
}
