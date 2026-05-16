package common

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// AssetPrecision is the canonical mapping from internal asset symbol to the
// number of base-unit decimal places. Internal accounting stores amounts as
// int64 in base units; this registry is the single source of truth for how
// to convert to/from the human/L1 string form.
//
// Hive's L1 HBD and HIVE assets both have precision 3 (one HBD == 1000 base
// units). The savings/consensus variants share their backing asset's
// precision. New assets must be added here before they can flow through the
// asset-aware helpers.
var AssetPrecision = map[string]uint8{
	"hbd":            3,
	"hive":           3,
	"hbd_savings":    3,
	"hive_consensus": 3,
}

// GetAssetPrecision returns the registered precision for an asset symbol.
// ok=false for unknown assets — callers must decide whether that is fatal
// (consensus path) or recoverable (presentation).
func GetAssetPrecision(asset string) (uint8, bool) {
	p, ok := AssetPrecision[strings.ToLower(asset)]
	return p, ok
}

// ParseAssetAmount parses a decimal-string amount in the asset's natural
// human form (e.g. "1.234" for HBD/HIVE) and returns the value in base
// units. Padding shorter fractions and rejecting longer ones to avoid
// silent precision loss.
func ParseAssetAmount(amount, asset string) (int64, error) {
	prec, ok := GetAssetPrecision(asset)
	if !ok {
		return 0, fmt.Errorf("unknown asset %q", asset)
	}
	parts := strings.Split(strings.TrimSpace(amount), ".")
	switch len(parts) {
	case 1:
		// integer form — pad to base units
		n, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			return 0, err
		}
		mult := int64(1)
		for i := uint8(0); i < prec; i++ {
			mult *= 10
		}
		// Detect int64 overflow on n*mult before it silently wraps.
		if n != 0 && (n > math.MaxInt64/mult || n < math.MinInt64/mult) {
			return 0, fmt.Errorf("amount %q overflows int64 base units for %s", amount, asset)
		}
		return n * mult, nil
	case 2:
		intPart, fracPart := parts[0], parts[1]
		if len(fracPart) > int(prec) {
			return 0, fmt.Errorf("amount %q exceeds %s precision (%d)", amount, asset, prec)
		}
		// pad to exactly `prec` digits
		fracPart += strings.Repeat("0", int(prec)-len(fracPart))
		// strip a leading sign from intPart so we can re-attach after concat;
		// reject any further sign characters so "--1.000" / "+-1.000" don't slip through
		neg := false
		if strings.HasPrefix(intPart, "-") {
			neg = true
			intPart = intPart[1:]
		} else if strings.HasPrefix(intPart, "+") {
			intPart = intPart[1:]
		}
		if strings.ContainsAny(intPart, "+-") {
			return 0, fmt.Errorf("amount %q has malformed sign", amount)
		}
		joined := intPart + fracPart
		n, err := strconv.ParseInt(joined, 10, 64)
		if err != nil {
			return 0, err
		}
		if neg {
			n = -n
		}
		return n, nil
	default:
		return 0, fmt.Errorf("amount %q must contain at most one decimal point", amount)
	}
}

// FormatAssetAmount renders a base-unit int64 as the human/L1 decimal
// string for the given asset (e.g. 1234 + "hbd" -> "1.234"). Always pads
// the fractional part to the asset's precision.
func FormatAssetAmount(amount int64, asset string) (string, error) {
	prec, ok := GetAssetPrecision(asset)
	if !ok {
		return "", fmt.Errorf("unknown asset %q", asset)
	}
	if prec == 0 {
		return strconv.FormatInt(amount, 10), nil
	}
	// FormatInt then strip any leading '-' so we never rely on -amount, which
	// wraps to itself for math.MinInt64.
	s := strconv.FormatInt(amount, 10)
	neg := false
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}
	for len(s) <= int(prec) {
		s = "0" + s
	}
	cut := len(s) - int(prec)
	out := s[:cut] + "." + s[cut:]
	if neg {
		out = "-" + out
	}
	return out, nil
}
