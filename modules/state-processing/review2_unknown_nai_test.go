package state_engine_test

import (
	"testing"

	"vsc-node/modules/db/vsc/hive_blocks"

	"github.com/vsc-eco/hivego"
)

// review2 LOW #104 — in ProcessBlock's transfer parsing, an unrecognised
// NAI left token == "", but the op was still credited: Deposit stored a
// LedgerRecord with Asset == "". Junk/unknown-asset deposits to the
// gateway polluted the ledger with empty-asset records.
//
// Differential: on the #170 baseline an unknown-NAI gateway transfer
// produces a stored deposit record with Asset == "" (RED); on fix the op
// is skipped and no record is stored (GREEN). A well-formed HIVE deposit
// in the same block is still credited on both arms (sanity).
func TestReview2UnknownNaiNotCredited(t *testing.T) {
	te := newTestEnv()

	block := hive_blocks.HiveBlock{
		BlockNumber: 100,
		BlockID:     "0000006400000000000000000000000000000000",
		Timestamp:   "2020-01-01T00:00:00",
		Transactions: []hive_blocks.Tx{
			{
				Index:         0,
				TransactionID: "rev2-104-unknown",
				Operations: []hivego.Operation{{
					Type: "transfer",
					Value: map[string]interface{}{
						"from": "alice",
						"to":   "vsc.gateway",
						"memo": "",
						"amount": map[string]interface{}{
							"amount":    "1.000",
							"nai":       "@@999999999", // unrecognised NAI
							"precision": 3,
						},
					},
				}},
			},
			{
				Index:         1,
				TransactionID: "rev2-104-known",
				Operations: []hivego.Operation{{
					Type: "transfer",
					Value: map[string]interface{}{
						"from": "bob",
						"to":   "vsc.gateway",
						"memo": "",
						"amount": map[string]interface{}{
							"amount":    "2.000",
							"nai":       "@@000000021", // HIVE
							"precision": 3,
						},
					},
				}},
			},
		},
	}

	te.SE.ProcessBlock(block)

	// Unknown NAI: no record at all, and certainly none with Asset == "".
	for _, r := range te.LedgerDb.LedgerRecords["hive:alice"] {
		if r.Asset == "" {
			t.Fatalf("review2 #104: unknown-NAI deposit credited as empty-asset record: %+v "+
				"(baseline stores Asset==\"\", fix skips the op)", r)
		}
	}
	if got := len(te.LedgerDb.LedgerRecords["hive:alice"]); got != 0 {
		t.Fatalf("review2 #104: unknown-NAI deposit produced %d ledger record(s), want 0", got)
	}

	// Sanity: the well-formed HIVE deposit is still credited on both arms.
	bobRecs := te.LedgerDb.LedgerRecords["hive:bob"]
	hiveCredited := false
	for _, r := range bobRecs {
		if r.Asset == "hive" {
			hiveCredited = true
		}
	}
	if !hiveCredited {
		t.Fatalf("review2 #104: well-formed HIVE deposit not credited (records=%+v)", bobRecs)
	}
}
