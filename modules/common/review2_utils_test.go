package common_test

import (
	"testing"

	"vsc-node/modules/common"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// review2 MEDIUM #87 — ArrayToStringArray did `reflect.TypeOf(arr).String()`
// with no nil guard and bare `v.(string)` element asserts. A crafted L1
// custom_json that omits `required_auths` (→ nil) or carries a non-string
// element crashes block processing (state_engine.go:304 et al).
//
// Differential: #170 baseline panics on these inputs (RED); fix/review2
// returns safely (GREEN). Valid inputs must still round-trip.
func TestReview2ArrayToStringArray_NoPanicOnHostileInput(t *testing.T) {
	cases := []struct {
		name string
		in   interface{}
		want []string
	}{
		{"nil", nil, []string{}},
		{"non-string element in []interface{}", []interface{}{"a", 1, "b"}, []string{"a", "b"}},
		{"non-string element in primitive.A", primitive.A{"x", 2, "y"}, []string{"x", "y"}},
		{"valid []interface{}", []interface{}{"a", "b"}, []string{"a", "b"}},
		{"valid []string", []string{"c", "d"}, []string{"c", "d"}},
		{"valid primitive.A", primitive.A{"e"}, []string{"e"}},
		{"empty []interface{}", []interface{}{}, []string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("review2 #87: ArrayToStringArray(%v) panicked: %v", c.in, r)
				}
			}()
			got := common.ArrayToStringArray(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("got %v, want %v", got, c.want)
				}
			}
		})
	}
}
