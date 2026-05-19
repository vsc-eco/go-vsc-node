package dids

import (
	"bytes"
	"testing"
)

// buildWitnessStack serializes items using compact-size varints, matching the
// on-the-wire BIP-322 witness encoding: varint(num_items) [varint(len) bytes]...
func buildWitnessStack(items [][]byte) []byte {
	var buf bytes.Buffer
	buf.Write(writeVarInt(len(items)))
	for _, it := range items {
		buf.Write(writeVarInt(len(it)))
		buf.Write(it)
	}
	return buf.Bytes()
}

// TestParseCompactWitnessStack_LargeItem is the GO-19 A/B test.
//
// RED (pre-fix): the parser read item lengths as a single byte, so an item of
// length >= 253 (serialized with the 0xFD compact-size prefix) is mis-parsed:
// the 0xFD prefix byte is interpreted as length 253 and parsing fails or
// returns garbage.
//
// GREEN (post-fix): readVarInt decodes the 0xFD/0xFE/0xFF compact-size prefixes
// correctly and the original item is recovered.
func TestParseCompactWitnessStack_LargeItem(t *testing.T) {
	large := bytes.Repeat([]byte{0xAB}, 300) // 300 >= 253 -> 0xFD prefix
	small := []byte{0x01, 0x02, 0x03}        // < 253 -> single-byte length

	data := buildWitnessStack([][]byte{large, small})

	items, err := parseCompactWitnessStack(data)
	if err != nil {
		t.Fatalf("parseCompactWitnessStack returned error: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	if !bytes.Equal(items[0], large) {
		t.Fatalf("large item mismatch: got %d bytes, want %d bytes", len(items[0]), len(large))
	}
	if !bytes.Equal(items[1], small) {
		t.Fatalf("small item mismatch: got %v, want %v", items[1], small)
	}
}

// TestParseCompactWitnessStack_SmallItems is the regression guard: small
// (<253) length values must keep working (single-byte compact-size).
func TestParseCompactWitnessStack_SmallItems(t *testing.T) {
	a := []byte{0x10, 0x20}
	b := bytes.Repeat([]byte{0x07}, 64)

	data := buildWitnessStack([][]byte{a, b})

	items, err := parseCompactWitnessStack(data)
	if err != nil {
		t.Fatalf("parseCompactWitnessStack returned error: %v", err)
	}
	if len(items) != 2 || !bytes.Equal(items[0], a) || !bytes.Equal(items[1], b) {
		t.Fatalf("small-item parsing regressed: %v", items)
	}
}

// TestReadVarInt covers each compact-size width and truncation handling.
func TestReadVarInt(t *testing.T) {
	cases := []struct {
		name   string
		data   []byte
		want   uint64
		offEnd int
	}{
		{"single", []byte{0x42}, 0x42, 1},
		{"fd", []byte{0xFD, 0x01, 0x01}, 0x0101, 3},
		{"fe", []byte{0xFE, 0x04, 0x03, 0x02, 0x01}, 0x01020304, 5},
		{"ff", []byte{0xFF, 0x08, 0x07, 0x06, 0x05, 0x04, 0x03, 0x02, 0x01}, 0x0102030405060708, 9},
	}
	for _, c := range cases {
		v, off, err := readVarInt(c.data, 0)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", c.name, err)
		}
		if v != c.want || off != c.offEnd {
			t.Fatalf("%s: got (v=%d off=%d) want (v=%d off=%d)", c.name, v, off, c.want, c.offEnd)
		}
	}
	if _, _, err := readVarInt([]byte{0xFD, 0x01}, 0); err == nil {
		t.Fatalf("expected truncation error for short 0xFD varint")
	}
}
