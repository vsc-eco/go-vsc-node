package state_engine_test

import (
	"testing"

	"vsc-node/modules/db/vsc/hive_blocks"

	"github.com/vsc-eco/hivego"
)

// review2 MED #86/#88 (sweep, milo review follow-up) — ProcessBlock
// also parsed custom_json system/header/user ops with bare assertions
// (opVal["id"].(string), opVal["json"].(string)) and account_update
// with opValue["json_metadata"].(string) / ["account"].(string).
// A custom_json whose `id` or `json` is not a string (untrusted L1
// payload, or a future L1 divergence) panicked "interface conversion"
// and crashed block processing for every node.
//
// Differential: pre-sweep panics inside ProcessBlock (RED); post-sweep
// comma-oks and skips just that op (GREEN).
func TestReview2ProcessBlockMalformedCustomJsonNoPanic(t *testing.T) {
	te := newTestEnv()

	block := hive_blocks.HiveBlock{
		BlockNumber: 101,
		BlockID:     "0000006500000000000000000000000000000000",
		Timestamp:   "2020-01-01T00:00:00",
		Transactions: []hive_blocks.Tx{
			{
				Index:         0,
				TransactionID: "rev2-86-cj-id-not-string",
				Operations: []hivego.Operation{{
					Type: "custom_json",
					Value: map[string]interface{}{
						// id is a number, not a string
						"id":             12345,
						"json":           `{"k":"v"}`,
						"required_auths": []interface{}{"alice"},
					},
				}},
			},
			{
				Index:         1,
				TransactionID: "rev2-86-cj-json-not-string",
				Operations: []hivego.Operation{{
					Type: "custom_json",
					Value: map[string]interface{}{
						"id":                     "vsc.actions",
						"json":                   map[string]interface{}{"not": "a string"},
						"required_auths":         []interface{}{"alice"},
						"required_posting_auths": []interface{}{},
					},
				}},
			},
			{
				Index:         2,
				TransactionID: "rev2-86-acctupdate-meta-not-string",
				Operations: []hivego.Operation{{
					Type: "account_update",
					Value: map[string]interface{}{
						"account":       "alice",
						"json_metadata": 42, // not a string
					},
				}},
			},
		},
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("review2 #86/#88: ProcessBlock panicked on malformed custom_json/account_update op: %v", r)
		}
	}()

	te.SE.ProcessBlock(block)
}
