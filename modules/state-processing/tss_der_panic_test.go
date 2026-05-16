package state_engine_test

import (
	"encoding/json"
	"testing"

	"vsc-node/modules/db/vsc/hive_blocks"
	tss_db "vsc-node/modules/db/vsc/tss"

	"github.com/vsc-eco/hivego"
)

// End-to-end reproduction of pentest finding F19.
//
// Bug: in state_engine.ProcessBlock's vsc.tss_sign handler, the
// returned err from btcec.ParseDERSignature was unchecked. Garbage
// DER bytes from a committee member made ParseDERSignature return
// (nil, err); the next line called signature.Verify on the nil
// receiver, which nil-deref panics. The recover() in the production
// hive_blocks listener catches the panic but kills the listener
// goroutine, halting block processing.
//
// This test drives the actual ProcessBlock entry point (no library
// invariants in isolation) and asserts:
//  1. ProcessBlock does NOT panic on a malformed DER signature.
//  2. The pending TssRequest in MongoDB is NOT advanced to
//     SignComplete (the signature was invalid; nothing should be
//     saved).
//
// Pre-fix: ProcessBlock panics — the deferred recover in this test
// captures it, panicValue is non-nil, the first assertion fails.
//
// Post-fix: ProcessBlock skips the bad sigPack, the request stays
// SignPending, and both assertions pass.
func TestF19_ProcessBlockSurvivesGarbageDERSignature(t *testing.T) {
	te := newTestEnv()

	const keyID = "vsc1Test-primary"

	// Stage state: an active ECDSA key with a valid public key, plus
	// a pending request waiting for a signature.
	te.TssKeys.Keys[keyID] = tss_db.TssKey{
		Id:        keyID,
		Status:    tss_db.TssKeyActive,
		Algo:      tss_db.EcdsaType,
		PublicKey: "023938e0de9a95c261f457c7851bf7ad928f740b633ade33e608d226f9cccf0912",
	}
	te.TssRequests.Requests["req-1"] = tss_db.TssRequest{
		Id:     "req-1",
		KeyId:  keyID,
		Msg:    "deadbeef",
		Status: tss_db.SignPending,
	}

	// Build a vsc.tss_sign custom_json carrying garbage DER bytes.
	signPayload, err := json.Marshal(map[string]interface{}{
		"packet": []map[string]string{
			{
				"key_id": keyID,
				"msg":    "deadbeef",
				// 8 bytes of garbage. ParseDERSignature returns
				// (nil, err) for this on the unfixed code path.
				"sig": "deadbeefcafebabe",
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	tx := hive_blocks.Tx{
		TransactionID: "f19-test-tx",
		Operations: []hivego.Operation{{
			Type: "custom_json",
			Value: map[string]interface{}{
				"id":                     "vsc.tss_sign",
				"json":                   string(signPayload),
				"required_auths":         []string{"someone"},
				"required_posting_auths": []string{},
			},
		}},
	}

	block := hive_blocks.HiveBlock{
		BlockNumber:  1000,
		BlockID:      "f19-test-block",
		MerkleRoot:   "fake-merkle",
		Transactions: []hive_blocks.Tx{tx},
		Timestamp:    "2026-05-07T00:00:00Z",
	}

	// Drive ProcessBlock synchronously so we can observe any panic.
	// The production listener (hive_blocks.ListenToBlockUpdates)
	// also catches the panic via recover(), but only as a way to
	// terminate the listener goroutine — block processing dies.
	var panicValue interface{}
	func() {
		defer func() {
			if r := recover(); r != nil {
				panicValue = r
			}
		}()
		te.SE.ProcessBlock(block)
	}()

	if panicValue != nil {
		t.Fatalf("ProcessBlock panicked on garbage TSS DER signature: %v\n"+
			"This is the F19 bug: btcec.ParseDERSignature returns (nil, err); "+
			"calling signature.Verify on the nil receiver nil-derefs.",
			panicValue)
	}

	got := te.TssRequests.Requests["req-1"]
	if got.Status == tss_db.SignComplete {
		t.Errorf("TssRequest was incorrectly marked SignComplete with a garbage signature: %+v", got)
	}
	if got.Sig != "" {
		t.Errorf("TssRequest sig was populated with garbage: %q", got.Sig)
	}
}
