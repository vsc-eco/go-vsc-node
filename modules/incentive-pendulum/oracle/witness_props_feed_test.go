package oracle

import (
	"encoding/binary"
	"encoding/hex"
	"testing"

	"vsc-node/modules/db/vsc/hive_blocks"

	"github.com/vsc-eco/hivego"
)

// realSteempeakExchangeRate is the verbatim hbd_exchange_rate prop value pulled
// from a live mainnet steempeak witness_set_properties op:
//   base  = 0.061 SBD  (HBD), quote = 1.000 STEEM (HIVE)  ->  0.061 HBD/HIVE
const realSteempeakExchangeRate = "3d000000000000000353424400000000e80300000000000003535445454d0000"

// legacyAssetHex builds one 16-byte legacy Hive asset (int64 LE amount +
// precision byte + 7-byte null-padded symbol) the way hived serializes it
// inside witness_set_properties props.
func legacyAssetHex(amount int64, precision byte, sym string) string {
	b := make([]byte, hiveAssetBytes)
	binary.LittleEndian.PutUint64(b[:8], uint64(amount))
	b[8] = precision
	copy(b[9:hiveAssetBytes], sym) // truncates/null-pads into the 7-byte field
	return hex.EncodeToString(b)
}

func priceHex(baseAmt int64, baseSym string, quoteAmt int64, quoteSym string) string {
	return legacyAssetHex(baseAmt, 3, baseSym) + legacyAssetHex(quoteAmt, 3, quoteSym)
}

// interestRateHex builds the fc-serialized uint16 LE hex hived emits for an
// hbd_interest_rate prop value.
func interestRateHex(bps uint16) string {
	b := make([]byte, 2)
	binary.LittleEndian.PutUint16(b, bps)
	return hex.EncodeToString(b)
}

func TestParseHbdInterestRate_Table(t *testing.T) {
	cases := []struct {
		name   string
		hexVal string
		wantOK bool
		want   int
	}{
		{"2000 bps", interestRateHex(2000), true, 2000}, // -> "d007"
		{"1500 bps", interestRateHex(1500), true, 1500}, // -> "dc05"
		{"zero", interestRateHex(0), true, 0},
		{"max uint16", interestRateHex(65535), true, 65535},
		{"one byte rejected", "0a", false, 0},
		{"three bytes rejected", "0a0000", false, 0},
		{"non-hex rejected", "zzzz", false, 0},
		{"empty rejected", "", false, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, ok := parseHbdInterestRate(c.hexVal)
			if ok != c.wantOK || (ok && v != c.want) {
				t.Fatalf("v=%d ok=%v want v=%d ok=%v", v, ok, c.want, c.wantOK)
			}
		})
	}
}

func TestParseHbdExchangeRate_RealMainnetSteempeak(t *testing.T) {
	// The hand-rolled builder must reproduce the on-chain bytes exactly.
	if got := priceHex(61, "SBD", 1000, "STEEM"); got != realSteempeakExchangeRate {
		t.Fatalf("builder mismatch:\n got %s\nwant %s", got, realSteempeakExchangeRate)
	}
	q, ok := parseHbdExchangeRate(realSteempeakExchangeRate, true)
	if !ok {
		t.Fatal("expected real mainnet op to decode")
	}
	if q.HbdRaw != 61 || q.HiveRaw != 1000 {
		t.Fatalf("q=%+v want {HbdRaw:61 HiveRaw:1000}", q)
	}
	if q.PriceBps() != 610 { // 0.061 HBD/HIVE
		t.Fatalf("priceBps=%d want 610", q.PriceBps())
	}
}

func TestParseHbdExchangeRate_Table(t *testing.T) {
	cases := []struct {
		name      string
		hexVal    string
		onMainnet bool
		wantOK    bool
		wantHbd   int64
		wantHive  int64
	}{
		{"legacy sbd/steem", priceHex(62, "SBD", 1000, "STEEM"), true, true, 62, 1000},
		{"modern hbd/hive", priceHex(62, "HBD", 1000, "HIVE"), true, true, 62, 1000},
		{"reversed legs hive-first", priceHex(1000, "STEEM", 62, "SBD"), true, true, 62, 1000},
		{"testnet tbd/tests off-mainnet", priceHex(62, "TBD", 1000, "TESTS"), false, true, 62, 1000},
		{"testnet tbd/tests rejected on mainnet", priceHex(62, "TBD", 1000, "TESTS"), true, false, 0, 0},
		{"both hbd rejected", priceHex(62, "SBD", 1000, "HBD"), true, false, 0, 0},
		{"unknown symbol rejected", priceHex(62, "DOGE", 1000, "STEEM"), true, false, 0, 0},
		{"zero amount rejected", priceHex(0, "SBD", 1000, "STEEM"), true, false, 0, 0},
		{"wrong length rejected", "abcd", true, false, 0, 0},
		{"odd hex rejected", "abc", true, false, 0, 0},
		{"empty rejected", "", true, false, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q, ok := parseHbdExchangeRate(c.hexVal, c.onMainnet)
			if ok != c.wantOK {
				t.Fatalf("ok=%v want %v (q=%+v)", ok, c.wantOK, q)
			}
			if ok && (q.HbdRaw != c.wantHbd || q.HiveRaw != c.wantHive) {
				t.Fatalf("q=%+v want {Hbd:%d Hive:%d}", q, c.wantHbd, c.wantHive)
			}
		})
	}
}

func TestParseHbdExchangeRate_WrongPrecisionRejected(t *testing.T) {
	// precision 2 on the base leg must be rejected (raw amounts would no longer
	// be directly comparable across legs).
	bad := legacyAssetHex(62, 2, "SBD") + legacyAssetHex(1000, 3, "STEEM")
	if _, ok := parseHbdExchangeRate(bad, true); ok {
		t.Fatal("expected precision != 3 to be rejected")
	}
}

// TestFeedTrackerTick_WitnessSetPropertiesPrice is the end-to-end proof of the
// fix: a witness who publishes price ONLY via witness_set_properties (no
// feed_publish, exactly the steempeak case) still produces a trusted price.
func TestFeedTrackerTick_WitnessSetPropertiesPrice(t *testing.T) {
	tr := NewFeedTracker(true) // mainnet symbols (SBD/STEEM)
	for h := uint64(1); h <= 4; h++ {
		tr.RecordWitnessBlock("steempeak")
		tr.IngestTransactionOps(h, hive_blocks.Tx{
			Operations: []hivego.Operation{{
				Type: "witness_set_properties",
				Value: map[string]interface{}{
					"owner": "steempeak",
					"props": []interface{}{
						[]interface{}{"hbd_exchange_rate", realSteempeakExchangeRate},
					},
				},
			}},
		})
	}
	tr.TickIfDue(100)
	snap := tr.LastTick()
	if !snap.TrustedHiveOK {
		t.Fatal("expected trusted hive price from witness_set_properties feed")
	}
	if snap.TrustedHivePriceBps != 610 { // 0.061 HBD/HIVE
		t.Fatalf("priceBps=%d want 610", snap.TrustedHivePriceBps)
	}
	if len(snap.TrustedWitnessGroup) != 1 || snap.TrustedWitnessGroup[0] != "steempeak" {
		t.Fatalf("group=%v", snap.TrustedWitnessGroup)
	}
}

// TestFeedTrackerLastWriteWins confirms feed_publish and witness_set_properties
// for the same witness resolve deterministically by block/op order — the later
// write wins, so all nodes replaying the same stream converge.
func TestFeedTrackerLastWriteWins(t *testing.T) {
	tr := NewFeedTracker(true)
	for h := uint64(1); h <= 4; h++ {
		tr.RecordWitnessBlock("steempeak")
	}
	// feed_publish first (0.25), then witness_set_properties (0.061) later in
	// the same block — the property price must win.
	tr.IngestTransactionOps(4, hive_blocks.Tx{
		Operations: []hivego.Operation{
			{Type: "feed_publish", Value: map[string]interface{}{
				"publisher": "steempeak",
				"exchange_rate": map[string]interface{}{
					"base": "0.250 HBD", "quote": "1.000 HIVE",
				},
			}},
			{Type: "witness_set_properties", Value: map[string]interface{}{
				"owner": "steempeak",
				"props": []interface{}{
					[]interface{}{"hbd_exchange_rate", realSteempeakExchangeRate},
				},
			}},
		},
	})
	tr.TickIfDue(100)
	if got := tr.LastTick().TrustedHivePriceBps; got != 610 {
		t.Fatalf("priceBps=%d want 610 (witness_set_properties should win)", got)
	}
}
