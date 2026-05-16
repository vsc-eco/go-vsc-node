package dids_test

import (
	"math/big"
	"testing"

	"vsc-node/lib/dids"
)

// review2 MEDIUM #103 — ConvertToEIP712TypedData ranged
// data.(map[string]interface{}) directly. When data is a struct, the !ok
// branch above it builds dataMap via json marshal/unmarshal, but the loop
// re-asserted the original struct as a map → "interface conversion:
// interface {} is <struct>, not map[string]interface{}" panic. Structs
// are a normal input shape (tx containers are passed as typed values), so
// any caller hitting the struct path crashed.
//
// Differential: #170 baseline panics on the struct input (RED);
// fix/review2 ranges dataMap and returns valid TypedData (GREEN). The map
// input is unchanged on both arms (sanity).
func TestReview2ConvertToEIP712TypedDataStruct(t *testing.T) {
	floatHandler := func(f float64) (*big.Int, error) {
		return big.NewInt(int64(f)), nil
	}

	type payload struct {
		To     string `json:"to"`
		Amount string `json:"amount"`
	}

	// struct input — baseline panics here, fix handles it via dataMap.
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("review2 #103: ConvertToEIP712TypedData(struct) panicked: %v", r)
			}
		}()
		td, err := dids.ConvertToEIP712TypedData(
			"vsc.network",
			payload{To: "hive:alice", Amount: "1.000"},
			"tx_container_v0",
			floatHandler,
		)
		if err != nil {
			t.Fatalf("review2 #103: struct input returned error: %v", err)
		}
		if td.Data.PrimaryType != "tx_container_v0" {
			t.Fatalf("review2 #103: primary type = %q, want tx_container_v0", td.Data.PrimaryType)
		}
		if got, ok := td.Data.Message["to"]; !ok || got != "hive:alice" {
			t.Fatalf("review2 #103: message[to] = %v (ok=%v), want hive:alice", got, ok)
		}
	}()

	// map input — sanity, identical on both arms.
	td, err := dids.ConvertToEIP712TypedData(
		"vsc.network",
		map[string]interface{}{"to": "hive:bob", "amount": "2.000"},
		"tx_container_v0",
		floatHandler,
	)
	if err != nil {
		t.Fatalf("review2 #103: map input returned error: %v", err)
	}
	if got, ok := td.Data.Message["to"]; !ok || got != "hive:bob" {
		t.Fatalf("review2 #103: map message[to] = %v (ok=%v), want hive:bob", got, ok)
	}
}
