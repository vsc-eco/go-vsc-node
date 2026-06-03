package oracle

import (
	"encoding/binary"
	"encoding/hex"
	"strings"
)

// hiveAssetBytes is the fixed wire size of a legacy Hive asset as it appears
// inside a witness_set_properties prop value: an int64 amount (8 bytes,
// little-endian) + 1 precision byte + 7 bytes of null-padded ASCII symbol
// name. A `price` (base + quote) is therefore exactly 2*hiveAssetBytes.
const hiveAssetBytes = 16

// exchangeRateFromProps pulls the witness_set_properties "hbd_exchange_rate"
// prop (if present) and decodes it into the same rational HBD-per-HIVE Quote
// the feed_publish path produces. Mirrors interestRateFromProps so both consume
// the identical flattened [key,value] view of the props encoding (plain nested
// arrays or the BSON field-prefix maps — flattenPropsPairs normalizes both).
func exchangeRateFromProps(props any, onMainnet bool) (Quote, bool) {
	for _, p := range flattenPropsPairs(props) {
		if strings.EqualFold(strings.TrimSpace(p[0]), "hbd_exchange_rate") {
			return parseHbdExchangeRate(p[1], onMainnet)
		}
	}
	return Quote{}, false
}

// interestRateFromProps pulls the witness_set_properties "hbd_interest_rate"
// prop (if present) and decodes it. Mirrors exchangeRateFromProps: it consumes
// the same flattened [key,value] view of the props and expects the raw
// fc-serialized value block_api delivers.
func interestRateFromProps(props any) (int, bool) {
	for _, p := range flattenPropsPairs(props) {
		if strings.EqualFold(strings.TrimSpace(p[0]), "hbd_interest_rate") {
			return parseHbdInterestRate(p[1])
		}
	}
	return 0, false
}

// parseHbdInterestRate decodes a witness_set_properties "hbd_interest_rate"
// prop value — a hex-encoded fc-serialized uint16 (little-endian, basis
// points). block_api delivers props values as raw fc bytes (the same op
// carries hbd_exchange_rate and the signing key as raw hex), so the prior
// decimal strconv.Atoi parse never matched real chain data and silently left
// the contract-exposed APR unpopulated on mainnet. The downstream
// MaxHBDInterestRateBps ceiling in HBDAPRModeFromGroup validates magnitude;
// here we only require a well-formed 2-byte value. Pure byte math —
// deterministic across nodes.
func parseHbdInterestRate(rawHex string) (int, bool) {
	b, err := hex.DecodeString(strings.TrimSpace(rawHex))
	if err != nil || len(b) != 2 {
		return 0, false
	}
	return int(binary.LittleEndian.Uint16(b)), true
}

// parseHbdExchangeRate decodes a witness_set_properties "hbd_exchange_rate"
// prop value — a hex-encoded fc-serialized `price` — into a Quote.
//
// Wire format (confirmed against a real mainnet steempeak op,
// "3d000000000000000353424400000000e80300000000000003535445454d0000"): 32
// bytes = two back-to-back legacy assets, base then quote, each:
//
//	[0:8]  int64 amount, little-endian
//	[8]    precision (HBD/HIVE are always 3)
//	[9:16] symbol name, ASCII, null-padded
//
// The on-chain binary symbol names are the pre-fork "SBD"/"STEEM" (HBD and
// HIVE retained their original serialized symbols across the Steem→Hive fork),
// so those are accepted on every network; testnet "TBD"/"TESTS" are accepted
// only off-mainnet, mirroring the onMainnet gating on ingestFeedPublish.
// base/quote may appear in either order — legs are matched by symbol, not
// position. Returns (_, false) on any malformed, non-HBD/HIVE, wrong-precision,
// or non-positive input so a bad prop is skipped rather than feeding a corrupt
// quote into the consensus-bound trusted-price aggregation.
//
// Determinism: pure byte math (hex decode + little-endian integer reads + ASCII
// symbol compare), no floats and no map iteration, so identical bytes yield an
// identical Quote on every node.
func parseHbdExchangeRate(rawHex string, onMainnet bool) (Quote, bool) {
	b, err := hex.DecodeString(strings.TrimSpace(rawHex))
	if err != nil || len(b) != 2*hiveAssetBytes {
		return Quote{}, false
	}
	baseAmt, baseSym, ok := decodeLegacyAsset(b[:hiveAssetBytes])
	if !ok {
		return Quote{}, false
	}
	quoteAmt, quoteSym, ok := decodeLegacyAsset(b[hiveAssetBytes:])
	if !ok {
		return Quote{}, false
	}
	baseLeg := classifyHiveSymbol(baseSym, onMainnet)
	quoteLeg := classifyHiveSymbol(quoteSym, onMainnet)
	// Require exactly one HBD leg and one HIVE leg, in either order.
	switch {
	case baseLeg == legHBD && quoteLeg == legHIVE:
		return Quote{HbdRaw: baseAmt, HiveRaw: quoteAmt}, true
	case baseLeg == legHIVE && quoteLeg == legHBD:
		return Quote{HbdRaw: quoteAmt, HiveRaw: baseAmt}, true
	default:
		return Quote{}, false
	}
}

// decodeLegacyAsset decodes one 16-byte legacy Hive asset into its raw amount
// and lowercase symbol name. Enforces precision == 3 (the HBD/HIVE invariant
// the Quote math relies on, so the raw amounts of two legs are directly
// comparable integers) and amount > 0.
func decodeLegacyAsset(b []byte) (amount int64, symbol string, ok bool) {
	if len(b) != hiveAssetBytes {
		return 0, "", false
	}
	amt := int64(binary.LittleEndian.Uint64(b[:8]))
	if amt <= 0 {
		return 0, "", false
	}
	if b[8] != 3 { // precision
		return 0, "", false
	}
	name := strings.ToLower(strings.TrimRight(string(b[9:hiveAssetBytes]), "\x00"))
	if name == "" {
		return 0, "", false
	}
	return amt, name, true
}

type hiveLeg int

const (
	legNone hiveLeg = iota
	legHBD
	legHIVE
)

// classifyHiveSymbol maps a decoded asset symbol name to its leg. The legacy
// binary names "sbd"/"steem" are the actual mainnet on-chain forms for
// HBD/HIVE and are accepted everywhere; testnet "tbd"/"tests" are accepted only
// off-mainnet, matching the onMainnet gating on the feed_publish path so a
// mainnet node won't silently accept a foreign-symbol feed.
func classifyHiveSymbol(sym string, onMainnet bool) hiveLeg {
	switch sym {
	case "hbd", "sbd":
		return legHBD
	case "hive", "steem":
		return legHIVE
	}
	if !onMainnet {
		switch sym {
		case "tbd":
			return legHBD
		case "tests":
			return legHIVE
		}
	}
	return legNone
}
