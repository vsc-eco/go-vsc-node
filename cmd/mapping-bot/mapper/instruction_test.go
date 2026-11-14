package mapper

import (
	"strings"
	"testing"

	"github.com/btcsuite/btcd/chaincfg"
)

// A deposit swap names where it goes if it does not complete (refund_to); the
// mapping contract holds it for a manual refund otherwise. The bot refuses to
// issue an address for an instruction without a usable one.
func TestValidateInstruction_RefundTo(t *testing.T) {
	const btcMain = "bc1qar0srrr7xfkvy5l643lydnw9re59gtzzwf5mdq"
	const did = "did:pkh:bip122:000000000019d6689c085ae165831e93:" + btcMain
	const base = "swap_to=hive%3Ainnopay&swap_asset_out=hbd&destination_chain=HIVE"
	cases := []struct {
		name, instr string
		ok          bool
		errHas      string
	}{
		{"deposit needs no refund_to", "deposit_to=hive:alice", true, ""},
		{"swap without refund_to", base, false, "requires 'refund_to'"},
		{"hive account", base + "&refund_to=hive%3Aalice", true, ""},
		{"own bitcoin address", base + "&refund_to=" + btcMain, true, ""},
		{"own bitcoin DID", base + "&refund_to=" + did, true, ""},
		{"testnet address on mainnet", base + "&refund_to=tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx", false, "invalid refund_to"},
		{"an EVM account", base + "&refund_to=did:pkh:eip155:1:0x52908400098527886E0F7030069857D2E4169EE7", false, "invalid refund_to"},
		{"a contract", base + "&refund_to=contract:vsc1BdrQ6EtbQ64rq2PkPd21x4MaLnVRcJj85d", false, "invalid refund_to"},
		{"a bad hive name", base + "&refund_to=hive%3Aab.cde", false, "invalid refund_to"},
		{"EVM payout with a bitcoin refund", "swap_to=hive%3Abob&swap_asset_out=eth&destination_chain=ETH&refund_to=" + btcMain, false, "needs a hive: refund_to"},
		{"EVM payout with a hive refund", "swap_to=hive%3Abob&swap_asset_out=eth&destination_chain=ETH&refund_to=hive%3Abob", true, ""},
		{"swap_to a contract", "swap_to=contract:vsc1BdrQ6EtbQ64rq2PkPd21x4MaLnVRcJj85d&swap_asset_out=hbd&refund_to=hive%3Aalice", false, "not a user account"},
		{"swap_to a system account", "swap_to=system:refund-hold&swap_asset_out=hbd&refund_to=hive%3Aalice", false, "not a user account"},
		{"swap_to a Bitcoin DID", "swap_to=" + did + "&swap_asset_out=hbd&refund_to=hive%3Aalice", true, ""},
		{"deposit_to a contract stays allowed", "deposit_to=contract:vsc1BdrQ6EtbQ64rq2PkPd21x4MaLnVRcJj85d", true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateInstruction(c.instr, true, &chaincfg.MainNetParams)
			if c.ok && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if !c.ok && (err == nil || !strings.Contains(err.Error(), c.errHas)) {
				t.Fatalf("got %v, want an error containing %q", err, c.errHas)
			}
		})
	}
	// Without chain params only a hive: refund_to is usable.
	if err := ValidateInstruction(base+"&refund_to="+btcMain, true, nil); err == nil {
		t.Fatal("a chain address cannot be checked without chain params; it must be refused")
	}
}

func TestValidHiveName(t *testing.T) {
	for name, want := range map[string]bool{
		"innopay": true, "lordbutterfly": true, "magi.test1": true, "a-b-c": true, "abc": true,
		"ab": false, "ab.cde": false, "1abc": false, "abc-": false, "a--bc": false, "ABC": false,
		"seventeen-chars-x": false, "abc.de": false, "abc.def": true, "abc_def": false,
	} {
		if got := ValidHiveName(name); got != want {
			t.Errorf("%q: %v, want %v", name, got, want)
		}
	}
	if err := ValidateInstruction("deposit_to=hive:ab.cde", true, nil); err == nil {
		t.Error("a deposit to a name Hive refuses must be refused")
	}
}
