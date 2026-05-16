package state_engine

import (
	"testing"

	"github.com/vsc-eco/hivego"
)

// review2 MEDIUM #88 — hasFeePaymentOp asserted the fee-payment op's
// amount/from with bare type assertions. The fee op is the second op of
// an attacker-shaped system transaction; a transfer whose `amount` is a
// scalar (or with a non-string from) panicked block processing.
//
// Differential: #170 baseline panics on the malformed second op (RED);
// fix/review2 comma-oks and returns (false,0,"") (GREEN). A well-formed
// fee op is still detected on both arms (sanity).
func TestReview2HasFeePaymentOpMalformed(t *testing.T) {
	malformed := []hivego.Operation{
		{Type: "custom_json", Value: map[string]interface{}{"id": "vsc.x"}},
		{Type: "transfer", Value: map[string]interface{}{
			"to":   "vsc.gateway",
			"from": "alice",
			// amount is a scalar, not the {amount,nai} object asserted.
			"amount": "1000 HBD",
		}},
	}

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("review2 #88: hasFeePaymentOp panicked on malformed fee op: %v", r)
			}
		}()
		ok, amt, from := hasFeePaymentOp(malformed, 1, "hbd")
		if ok || amt != 0 || from != "" {
			t.Fatalf("review2 #88: malformed op accepted: ok=%v amt=%d from=%q", ok, amt, from)
		}
	}()

	// Sanity: a well-formed fee op is still recognised (identical both arms).
	good := []hivego.Operation{
		{Type: "custom_json", Value: map[string]interface{}{"id": "vsc.x"}},
		{Type: "transfer", Value: map[string]interface{}{
			"to":   "vsc.gateway",
			"from": "alice",
			"amount": map[string]any{
				"amount": "1000",
				"nai":    "@@000000013",
			},
		}},
	}
	ok, amt, from := hasFeePaymentOp(good, 1, "hbd")
	if !ok || amt != 1000 || from != "alice" {
		t.Fatalf("review2 #88: well-formed fee op not recognised: ok=%v amt=%d from=%q", ok, amt, from)
	}
}
