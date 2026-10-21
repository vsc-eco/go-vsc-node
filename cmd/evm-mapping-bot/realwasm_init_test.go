//go:build evm_realwasm

package main

import (
	"os"
	"strings"
	"testing"

	"vsc-node/lib/test_utils"
)

// D6 on the node's wasm runtime: initContract wires the verifier and requests
// the bridge's TSS key at deploy, so a fresh bridge does not wait out two
// 400K-block timelocks before its first deposit; a second init is refused.
func TestRealWasm_InitWiresTheVerifierAndKey(t *testing.T) {
	path := os.Getenv("EVM_MAPPING_WASM")
	if path == "" {
		t.Skip("EVM_MAPPING_WASM not set")
	}
	wasm, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c := test_utils.NewContractTest()
	ct := &c
	ct.RegisterContract(rwBridge, "hive:owner", wasm)

	r := rwCall(ct, "hive:owner", "initContract", []byte(`{"chain_id":11155111,"verifier_contract":"`+rwVerifier+`","create_key":true}`))
	if !r.Success {
		t.Fatalf("initContract: %s: %s", r.Err, r.ErrMsg)
	}
	if v := ct.StateGet(rwBridge, "zkverifier"); v != rwVerifier {
		t.Fatalf("verifier after init = %q, want %s", v, rwVerifier)
	}
	created := false
	for _, op := range r.Logs[rwBridge].TssOps {
		created = created || (op.Type == "create" && op.KeyId == rwBridge+"-primary" && op.Args == "ecdsa")
	}
	if !created {
		t.Fatalf("initContract requested no primary ecdsa key: %+v", r.Logs[rwBridge].TssOps)
	}

	r = rwCall(ct, "hive:owner", "initContract", []byte(`{"chain_id":11155111,"verifier_contract":"vsc1other"}`))
	if r.Success || !strings.Contains(r.ErrMsg, "already initialized") {
		t.Fatalf("second initContract: success=%v %s", r.Success, r.ErrMsg)
	}
	if v := ct.StateGet(rwBridge, "zkverifier"); v != rwVerifier {
		t.Fatalf("verifier after a refused second init = %q", v)
	}
}
