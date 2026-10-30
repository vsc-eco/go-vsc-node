package mapper

import (
	"encoding/hex"
	"testing"
)

// Records read from the shared testnet vault on 2026-10-06.
func TestUtxoRecordGeneration(t *testing.T) {
	for _, tc := range []struct {
		name string
		hex  string
		want uint32
	}{
		// u-5: legacy change output, written before generations (80 bytes, no tail).
		{"pre-generation legacy coin is generation 0",
			"ef670004117ec4c8ce272017c20795b1665390191bc03b694c6a93072998cb1400000001000000000000027c220020ba0b4e41a0f3cd89141c588d0f5d5c6b03e3ac7d158edacb6f1d71a5d1b95d6900", 0},
		// u-1048: migration output into gen 1 (untagged, 4-byte tail).
		{"untagged generation-1 coin",
			"228ed641f28cf0b4acb80abfc9a76081c58e5aa61377505c3f23d00f1c6da40300000000000000000000054d2200208caf4ee506401b10f552c69c2d8e548d44e340a36a4860fd1f9ce640067b89540000000001", 1},
		// u-1049: tagged deposit into gen 0 (32-byte tag, 4-byte tail).
		{"tagged generation-0 deposit",
			"0ffca5f80b7dae1b16a4400515e2ea8e021ff74faba5727f3776416b780f03e5000000000000000000004e2022002013f032518398202382a5b82be219ab505bf99f20b45054d200cdeac48f3a3a3220e2133b6928d0e69a2ac4d093cab4bc3c1316fdbc99cda9db37c2cb8d353d7bc900000000", 0},
	} {
		raw, _ := hex.DecodeString(tc.hex)
		got, err := utxoRecordGeneration(raw)
		if err != nil || got != tc.want {
			t.Errorf("%s: got %d, %v; want %d", tc.name, got, err, tc.want)
		}
	}
	// A tail that is neither 0 nor 4 bytes is corrupt: refuse, as the contract does.
	raw, _ := hex.DecodeString("228ed641f28cf0b4acb80abfc9a76081c58e5aa61377505c3f23d00f1c6da40300000000000000000000054d2200208caf4ee506401b10f552c69c2d8e548d44e340a36a4860fd1f9ce640067b8954000000")
	if _, err := utxoRecordGeneration(raw); err == nil {
		t.Error("a 3-byte generation tail must be refused")
	}
}
