package state_engine_test

import (
	"testing"

	"vsc-node/modules/db/vsc/hive_blocks"

	"github.com/vsc-eco/hivego"
)

// review2 MEDIUM #86 — ProcessBlock parsed L1 transfer ops with bare type
// assertions (op.Value["amount"].(map[string]interface{}), .("string"),
// op.Value["from"].(string)). Hive op payloads are untrusted input to the
// node; a transfer whose `amount` is a scalar (or with missing from/memo)
// panicked "interface conversion" and crashed block processing for every
// node — a remotely-triggerable liveness break.
//
// Differential: #170 baseline panics inside ProcessBlock on the malformed
// transfer (RED); fix/review2 comma-oks and skips the op (GREEN). A
// well-formed deposit in the same block is unaffected.
func TestReview2ProcessBlockMalformedTransferNoPanic(t *testing.T) {
	te := newTestEnv()

	block := hive_blocks.HiveBlock{
		BlockNumber: 100,
		BlockID:     "0000006400000000000000000000000000000000",
		Timestamp:   "2020-01-01T00:00:00",
		Transactions: []hive_blocks.Tx{{
			Index:         0,
			TransactionID: "rev2-86-malformed",
			Operations: []hivego.Operation{{
				Type: "transfer",
				Value: map[string]interface{}{
					"from": "alice",
					"to":   "vsc.gateway",
					"memo": "",
					// amount is a scalar string, not the {amount,nai,precision}
					// object the code blindly asserted.
					"amount": "1.000 HIVE",
				},
			}},
		}},
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("review2 #86: ProcessBlock panicked on malformed transfer op: %v", r)
		}
	}()

	te.SE.ProcessBlock(block)
}
