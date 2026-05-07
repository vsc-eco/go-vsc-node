package common_test

import (
	"math"
	"strconv"
	"testing"
	"vsc-node/modules/common"
)

func TestParseAssetAmount(t *testing.T) {
	tests := []struct {
		in    string
		asset string
		want  int64
		err   bool
	}{
		{"1.234", "hbd", 1234, false},
		{"1.234", "HBD", 1234, false}, // case-insensitive symbol
		{"0.001", "hive", 1, false},
		{"10", "hbd", 10000, false},        // integer form padded
		{"10.0", "hbd", 10000, false},      // single-digit fraction padded
		{"10.00", "hbd", 10000, false},     // double-digit fraction padded
		{"10.000", "hbd", 10000, false},    // exact precision
		{"-1.234", "hbd", -1234, false},    // negative
		{"+1.234", "hbd", 1234, false},     // explicit positive
		{"0", "hbd", 0, false},
		{"0.000", "hbd", 0, false},
		{"1.2345", "hbd", 0, true}, // exceeds precision — must reject
		{"1.234", "wat", 0, true},  // unknown asset
		{"abc", "hbd", 0, true},    // not a number
		{"1.2.3", "hbd", 0, true},  // malformed
		{"--1.000", "hbd", 0, true},                         // double-negative rejected
		{"+-1.000", "hbd", 0, true},                         // mixed-sign rejected
		{strconv.FormatInt(math.MaxInt64, 10), "hbd", 0, true}, // integer-form base-unit overflow rejected
	}
	for _, tt := range tests {
		got, err := common.ParseAssetAmount(tt.in, tt.asset)
		if tt.err {
			if err == nil {
				t.Errorf("ParseAssetAmount(%q,%q) expected error, got %d", tt.in, tt.asset, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseAssetAmount(%q,%q) unexpected error: %v", tt.in, tt.asset, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseAssetAmount(%q,%q) = %d, want %d", tt.in, tt.asset, got, tt.want)
		}
	}
}

func TestFormatAssetAmount(t *testing.T) {
	tests := []struct {
		amount int64
		asset  string
		want   string
		err    bool
	}{
		{1234, "hbd", "1.234", false},
		{1, "hbd", "0.001", false},
		{1000, "hive", "1.000", false},
		{0, "hbd", "0.000", false},
		{-1234, "hbd", "-1.234", false},
		{10000, "hbd", "10.000", false},
		{100, "hbd", "0.100", false},
		{10, "hbd", "0.010", false},
		{math.MinInt64, "hbd", "-9223372036854775.808", false}, // INT64_MIN must not double-negate
		{1234, "wat", "", true},
	}
	for _, tt := range tests {
		got, err := common.FormatAssetAmount(tt.amount, tt.asset)
		if tt.err {
			if err == nil {
				t.Errorf("FormatAssetAmount(%d,%q) expected error, got %q", tt.amount, tt.asset, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("FormatAssetAmount(%d,%q) unexpected error: %v", tt.amount, tt.asset, err)
			continue
		}
		if got != tt.want {
			t.Errorf("FormatAssetAmount(%d,%q) = %q, want %q", tt.amount, tt.asset, got, tt.want)
		}
	}
}

func TestParseFormatRoundtrip(t *testing.T) {
	for _, asset := range []string{"hbd", "hive"} {
		for _, amount := range []int64{0, 1, 999, 1000, 1234, 1_000_000_000, -1234} {
			s, err := common.FormatAssetAmount(amount, asset)
			if err != nil {
				t.Fatalf("Format(%d,%q): %v", amount, asset, err)
			}
			got, err := common.ParseAssetAmount(s, asset)
			if err != nil {
				t.Fatalf("Parse(%q,%q): %v", s, asset, err)
			}
			if got != amount {
				t.Errorf("roundtrip(%d, %q) = %d via %q", amount, asset, got, s)
			}
		}
	}
}

func TestGetAssetPrecision(t *testing.T) {
	if p, ok := common.GetAssetPrecision("hbd"); !ok || p != 3 {
		t.Errorf("hbd precision = (%d,%v), want (3,true)", p, ok)
	}
	if p, ok := common.GetAssetPrecision("HIVE"); !ok || p != 3 {
		t.Errorf("HIVE precision = (%d,%v), want (3,true)", p, ok)
	}
	if _, ok := common.GetAssetPrecision("nonexistent"); ok {
		t.Errorf("nonexistent precision: expected ok=false")
	}
}
