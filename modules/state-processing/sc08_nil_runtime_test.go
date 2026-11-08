package state_engine

import (
	"encoding/json"
	"fmt"
	"testing"
)

// SC-08 regression.
//
// A `vsc.update_contract` custom_json whose `runtime` key is absent, or
// explicitly null, used to unmarshal to a nil *wasm_runtime.Runtime and then be
// dereferenced by tx.Runtime.String(). The same held for a missing
// `storage_proof` and StorageProof.Verify's read of sp.Hash. Either deref
// panicked inside ProcessBlock, which had no recover() on the L1 system-op
// path, so every node died on the block -- and died again on restart, because
// the streamer's checkpoint only advances after a block completes.
//
// These tests assert the two halves of the fix:
//   - validateCodeUpdateFields rejects the payloads instead of panicking, and
//   - runSystemOpSafely contains a panic if some other handler ever has one.

// sc08Payloads are the exact wire payloads that reproduced the halt.
var sc08Payloads = []struct {
	name string
	json string
}{
	{"runtime key absent", `{"net_id":"vsc-mainnet","id":"vsc1xxx","code":"bafyCODE"}`},
	{"runtime explicitly null", `{"net_id":"vsc-mainnet","id":"vsc1xxx","code":"bafyCODE","runtime":null}`},
	{"storage_proof absent", `{"net_id":"vsc-mainnet","id":"vsc1xxx","code":"bafyCODE","runtime":"go"}`},
	{"storage_proof explicitly null", `{"net_id":"vsc-mainnet","id":"vsc1xxx","code":"bafyCODE","runtime":"go","storage_proof":null}`},
}

func TestSC08_NilPointerPayloadsAreRejectedNotPanicking(t *testing.T) {
	for _, p := range sc08Payloads {
		t.Run(p.name, func(t *testing.T) {
			var tx TxUpdateContract
			if err := json.Unmarshal([]byte(p.json), &tx); err != nil {
				t.Fatalf("payload did not even unmarshal: %v", err)
			}

			// Precondition: this payload really does produce a nil pointer.
			// If a future struct change makes these value types, this test is
			// no longer exercising the bug and should be revisited.
			if tx.Runtime != nil && tx.StorageProof != nil {
				t.Fatalf("payload produced no nil pointer; test no longer covers SC-08")
			}

			err := validateReturnsErrNotPanic(t, &tx)
			if err == nil {
				t.Fatalf("validateCodeUpdateFields accepted a payload with a nil pointer field")
			}
			t.Logf("rejected cleanly: %v", err)
		})
	}
}

// validateReturnsErrNotPanic fails the test if validation panics rather than
// returning an error -- the whole point of the fix.
func validateReturnsErrNotPanic(t *testing.T, tx *TxUpdateContract) (err error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("validateCodeUpdateFields PANICKED (SC-08 not fixed): %v", r)
		}
	}()
	return tx.validateCodeUpdateFields()
}

// TestSC08_GuardIsWhatPreventsThePanic pins the causal claim: with the guard
// bypassed, the very expression ExecuteTx used to run does panic. If this ever
// stops panicking the nil-deref hazard is gone for another reason and the
// guard's comment should be corrected.
func TestSC08_GuardIsWhatPreventsThePanic(t *testing.T) {
	var tx TxUpdateContract
	if err := json.Unmarshal([]byte(sc08Payloads[0].json), &tx); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if tx.Runtime != nil {
		t.Fatalf("expected nil Runtime")
	}

	panicked := func() (p string) {
		defer func() {
			if r := recover(); r != nil {
				p = fmt.Sprint(r)
			}
		}()
		_ = tx.Runtime.String() // the unguarded expression from ExecuteTx
		return ""
	}()

	if panicked == "" {
		t.Fatalf("tx.Runtime.String() on a nil Runtime no longer panics; " +
			"SC-08's premise changed -- re-check the guard's rationale")
	}
	t.Logf("unguarded deref still panics as expected: %v", panicked)
}

// TestSC08_ValidPayloadAccepted is the control: a well-formed update must not
// be caught by the new guard.
func TestSC08_ValidPayloadAccepted(t *testing.T) {
	var tx TxUpdateContract
	payload := `{"net_id":"vsc-mainnet","id":"vsc1xxx","code":"bafyCODE","runtime":"go",` +
		`"storage_proof":{"hash":"bafyHASH","signature":{}}}`
	if err := json.Unmarshal([]byte(payload), &tx); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if tx.Runtime == nil || tx.StorageProof == nil {
		t.Fatalf("control payload should populate both pointers; runtime=%v proof=%v",
			tx.Runtime, tx.StorageProof)
	}
	if err := tx.validateCodeUpdateFields(); err != nil {
		t.Fatalf("valid payload rejected: %v", err)
	}
}

// TestSC08_RunSystemOpSafelyContainsPanic covers the class-level backstop: a
// handler that panics must not unwind into ProcessBlock.
func TestSC08_RunSystemOpSafelyContainsPanic(t *testing.T) {
	ran := false
	survived := func() (ok bool) {
		defer func() {
			if r := recover(); r != nil {
				ok = false
			}
		}()
		runSystemOpSafely("vsc.update_contract", TxSelf{TxId: "deadbeef", BlockHeight: 123}, func() {
			ran = true
			var p *TxUpdateContract
			_ = p.Id // deliberate nil deref, standing in for any handler panic
		})
		return true
	}()

	if !ran {
		t.Fatalf("handler was never invoked")
	}
	if !survived {
		t.Fatalf("runSystemOpSafely let a handler panic escape -- ProcessBlock would still die")
	}
}

// TestSC08_RunSystemOpSafelyPassesThroughSuccess makes sure the wrapper does not
// swallow normal execution or the handler's writes.
func TestSC08_RunSystemOpSafelyPassesThroughSuccess(t *testing.T) {
	got := UpdateContractResult{}
	runSystemOpSafely("vsc.update_contract", TxSelf{TxId: "feedface"}, func() {
		got = UpdateContractResult{Success: true, CodeUpdated: true}
	})
	if !got.Success || !got.CodeUpdated {
		t.Fatalf("wrapper dropped the handler's result: %+v", got)
	}
}
