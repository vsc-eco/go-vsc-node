package btcvault

import (
	"encoding/hex"
	"reflect"
	"testing"
)

// Vectors written by the BTC contract's own encoders (utxo-mapping 49e3679,
// mapping.MarshalUtxoRegistry / MarshalUtxo), so a change to either side's
// layout fails here.
func TestUtxoBookDecodesTheContractEncoding(t *testing.T) {
	reg, _ := hex.DecodeString("00070000000005dc040000000ee6b280ffff75f059e40090")
	got, err := UnmarshalUtxoRegistry(reg)
	// The registry keeps the lower 6 bytes of an amount (the contract's own
	// decoder returns the same): 2,099,999,997,690,000 comes back as
	// 129,675,160,715,408. Real UTXOs are far below 2^48 sats (2.8M BTC); the
	// watch takes amounts from the "u-" records, which keep all 8 bytes.
	want := []UtxoRegistryEntry{{7, 1500}, {1024, 250_000_000}, {65535, 129_675_160_715_408}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("registry %v %v, want %v", got, err, want)
	}
	if _, err := UnmarshalUtxoRegistry(reg[:7]); err == nil {
		t.Fatal("a registry that is not whole entries decoded")
	}

	rec, _ := hex.DecodeString("0f1e2d3c4b5a69788796a5b4c3d2e1f00112233445566778899aabbccddeeff000000003000000000ee6b280040020aabb20000000000000000000000000000000000000000000000000000000000000000000000002")
	op, err := UnmarshalUtxoOutpoint(rec)
	if err != nil || op != (UtxoOutpoint{TxId: "0f1e2d3c4b5a69788796a5b4c3d2e1f00112233445566778899aabbccddeeff0", Vout: 3, Amount: 250_000_000}) {
		t.Fatalf("record %+v %v", op, err)
	}
	rec2, _ := hex.DecodeString("aa000000000000000000000000000000000000000000000000000000000000010000000000000000000003e801510000000000")
	if op, err := UnmarshalUtxoOutpoint(rec2); err != nil || op.Vout != 0 || op.Amount != 1000 || op.TxId[:2] != "aa" {
		t.Fatalf("record 2 %+v %v", op, err)
	}
	if _, err := UnmarshalUtxoOutpoint(rec2[:44]); err == nil {
		t.Fatal("a truncated record decoded")
	}
	if UtxoKey(1024) != "u-400" || ReservedUtxoKey(1024) != "ru-1024" {
		t.Fatalf("keys %q %q", UtxoKey(1024), ReservedUtxoKey(1024))
	}
}
