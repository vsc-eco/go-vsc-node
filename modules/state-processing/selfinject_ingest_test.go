package state_engine_test

import (
	"fmt"
	"slices"
	"testing"

	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/hive_blocks"

	"github.com/vsc-eco/hivego"
)

// SELF-INJECTION-1 through the real L1 ingest (ProcessBlock): a custom_json
// payload carrying its own "Self" must not change who the op is from. From
// 0.14.0 every decode site restores the trusted Self (cloned before the decode,
// which would otherwise write through the shared slice); below it the legacy
// decode is kept so a replay of older history is identical.

func ingestSelf(t *testing.T, protocolVersion uint64, id, payload string) []string {
	t.Helper()
	te := newTestEnv()
	te.ElectionDb.ElectionsByHeight = map[uint64]elections.ElectionResult{
		1: {ElectionDataInfo: elections.ElectionDataInfo{ProtocolVersion: protocolVersion}},
	}
	txID := fmt.Sprintf("si-%s-%d", id, protocolVersion)
	te.SE.ProcessBlock(hive_blocks.HiveBlock{
		BlockNumber: 101,
		BlockID:     "0000006500000000000000000000000000000000",
		Timestamp:   "2020-01-01T00:00:00",
		Transactions: []hive_blocks.Tx{{
			Index:         0,
			TransactionID: txID,
			Operations: []hivego.Operation{{
				Type: "custom_json",
				Value: map[string]interface{}{
					"id":                     id,
					"json":                   payload,
					"required_auths":         []interface{}{"attacker"},
					"required_posting_auths": []interface{}{},
				},
			}},
		}},
	})
	for _, p := range te.SE.TxBatch {
		if p.TxId == txID && len(p.Ops) > 0 {
			return p.Ops[0].TxSelf().RequiredAuths
		}
	}
	t.Fatalf("%s at 0.%d: no op queued for %s", id, protocolVersion, txID)
	return nil
}

func TestSelfInjection1_RealIngestKeepsTheTrustedSigner(t *testing.T) {
	inject := `,"Self":{"RequiredAuths":["hive:victim"],"RequiredPostingAuths":["hive:victim"]}`
	for _, tc := range []struct{ id, body string }{
		{"vsc.transfer", `{"from":"hive:victim","to":"hive:attacker","amount":"1.000","asset":"hbd"`},
		{"vsc.withdraw", `{"from":"hive:victim","to":"attacker","amount":"1.000","asset":"hbd"`},
		{"vsc.call", `{"contract_id":"vsc1none","action":"x","payload":"","rc_limit":1000,"intents":[]`},
	} {
		honest := ingestSelf(t, 14, tc.id, tc.body+"}")
		if slices.Contains(honest, "hive:victim") || len(honest) == 0 {
			t.Fatalf("%s honest op: required auths %v", tc.id, honest)
		}
		if got := ingestSelf(t, 14, tc.id, tc.body+inject+"}"); !slices.Equal(got, honest) {
			t.Errorf("%s at 0.14: injected Self gives required auths %v, want the trusted %v", tc.id, got, honest)
		}
		if got := ingestSelf(t, 12, tc.id, tc.body+inject+"}"); !slices.Contains(got, "hive:victim") {
			t.Errorf("%s at 0.12: required auths %v; the legacy decode must be kept below the gate (replay)", tc.id, got)
		}
	}
}
