package state_engine_test

import (
	"encoding/json"
	"strings"
	"testing"

	"vsc-node/lib/test_utils"
	stateEngine "vsc-node/modules/state-processing"
)

// review2 LOW #70/#110 — TxVscCallContract.ExecuteTx only UTF-8 checked
// the contract-call payload; there was no explicit length cap, so the
// node implicitly trusted Hive's ~8KB custom_json limit to bound it. The
// fix enforces an explicit cap (params.MAX_CONTRACT_PAYLOAD_SIZE) before
// any contract fetch/execution, so the bound holds deterministically on
// every node regardless of ingest path.
//
// Differential: with an over-cap payload the fix returns "payload
// exceeds maximum size ..." before the contract lookup; the #170
// baseline has no such check and proceeds to "contract not found" (RED).
// A small payload yields "contract not found" on both arms (sanity — the
// cap does not reject normal calls).
func TestReview2ContractPayloadSizeCap(t *testing.T) {
	ct := test_utils.NewContractTest() // StateEngine NetId = vsc-mocknet

	call := func(payload []byte) string {
		tx := &stateEngine.TxVscCallContract{
			Self:       stateEngine.TxSelf{BlockHeight: 0},
			NetId:      "vsc-mocknet",
			ContractId: "does-not-exist",
			Action:     "noop",
			Payload:    json.RawMessage(payload),
		}
		return tx.ExecuteTx(ct.StateEngine, ct.LedgerSession, ct.RcSession, ct.CallSession, "").Ret
	}

	// Over the cap: fix rejects on size; baseline reaches contract lookup.
	big := []byte(strings.Repeat("a", 9*1024)) // 9216 > 8192
	if ret := call(big); !strings.Contains(ret, "payload exceeds maximum size") {
		t.Fatalf("review2 #70/#110: oversized payload Ret = %q, want 'payload exceeds maximum size' "+
			"(baseline has no cap and returns 'contract not found')", ret)
	}

	// Under the cap: the size guard is transparent — same on both arms.
	small := []byte(`{"x":1}`)
	if ret := call(small); !strings.Contains(ret, "contract not found") {
		t.Fatalf("review2 #70/#110: small payload Ret = %q, want 'contract not found' "+
			"(cap must not reject normal-sized calls)", ret)
	}
}
