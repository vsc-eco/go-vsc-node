package model

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// The scalars used to return strconv's own error text to the client and to
// wrap negative or oversized values silently (int64(-1) became
// 18446744073709551615). Both must now fail with a fixed message.

func TestUint64UnmarshalGQL(t *testing.T) {
	ok := map[string]any{
		"string":      "42",
		"uint64":      uint64(42),
		"int64":       int64(42),
		"json.Number": json.Number("42"),
	}
	for name, in := range ok {
		var u Uint64
		if err := u.UnmarshalGQL(in); err != nil || u != 42 {
			t.Errorf("%s: got %d, %v", name, u, err)
		}
	}

	bad := map[string]any{
		"garbage":      "not-a-uint",
		"negative str": "-1",
		"negative int": int64(-1),
		"overflow":     "18446744073709551616",
		"float":        json.Number("1.5"),
		"bool":         true,
		"nil":          nil,
	}
	for name, in := range bad {
		var u Uint64
		err := u.UnmarshalGQL(in)
		if err == nil {
			t.Errorf("%s: accepted, got %d", name, u)
			continue
		}
		if strings.Contains(err.Error(), "strconv") || strings.Contains(err.Error(), "type") {
			t.Errorf("%s: error leaks implementation detail: %q", name, err)
		}
	}
}

func TestInt64UnmarshalGQL(t *testing.T) {
	for name, in := range map[string]any{
		"string":   "-7",
		"int64":    int64(-7),
		"json.Num": json.Number("-7"),
	} {
		var i Int64
		if err := i.UnmarshalGQL(in); err != nil || i != -7 {
			t.Errorf("%s: got %d, %v", name, i, err)
		}
	}

	for name, in := range map[string]any{
		"garbage":       "abc",
		"overflow str":  "9223372036854775808",
		"overflow uint": uint64(math.MaxInt64) + 1,
		"bool":          false,
	} {
		var i Int64
		err := i.UnmarshalGQL(in)
		if err == nil {
			t.Errorf("%s: accepted, got %d", name, i)
			continue
		}
		if strings.Contains(err.Error(), "strconv") {
			t.Errorf("%s: error leaks strconv: %q", name, err)
		}
	}
}
