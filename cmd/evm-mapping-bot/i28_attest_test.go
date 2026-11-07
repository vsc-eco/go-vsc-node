package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"vsc-node/lib/btcvault"
)

// codeRPC answers eth_getProof with codeHash (and a one-node proof).
func codeRPC(t *testing.T, codeHash *atomic.Value) *ethRPC {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method != "eth_getProof" {
			t.Errorf("unexpected L1 call %s", req.Method)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{
			"codeHash": codeHash.Load().(string), "accountProof": []string{"0xc0"}, "nonce": "0x1", "balance": "0x0"}})
	}))
	t.Cleanup(srv.Close)
	return newEthRPC(srv.URL)
}

// I-28: the bot attests the active generation's MagiDeposit once its code is at
// the bridge's stored L1 height (gen, block_height = the stored height, the
// account proof), not while it has no code, and not again once attested.
func TestI28_BotAttestsADeployedMagiDeposit(t *testing.T) {
	l2, sub := poolWorld(t)
	l2.set("vsc1bridge", "zkverifier", "vsc1verifier")
	l2.set("vsc1verifier", "h", "120")
	l2.sim = func(action string) map[string]any {
		return map[string]any{"success": true, "ret": "attested MagiDeposit", "rc_used": 3000}
	}
	var codeHash atomic.Value
	codeHash.Store(emptyCodeHashHex)
	rpc := codeRPC(t, &codeHash)
	cfg := EVMBotConfig{ContractID: "vsc1bridge", NetID: "vsc-devnet", RcLimit: 10000}
	vault := "0x2b5ad5c4795c026514f8317c7a215e218dccd6cf"
	view := vaultView{Registry: true, Gens: []vaultGen{{Gen: 1, Status: btcvault.VaultStatusActive, Address: vault}}}
	view.Active = &view.Gens[0]
	old := depositAttestEvery
	depositAttestEvery = 0
	t.Cleanup(func() { depositAttestEvery = old })
	pass := func() {
		depositAttest.Lock()
		depositAttest.at = time.Time{}
		depositAttest.Unlock()
		attestDepositContracts(t.Context(), rpc, sub.gql, sub, cfg, view)
	}

	pass() // no code at the stored height
	if calls := submittedCalls(t, l2); len(calls) != 0 {
		t.Fatalf("attested a MagiDeposit with no code: %+v", calls)
	}
	codeHash.Store("0x" + strings.Repeat("ab", 32))
	pass()
	calls := submittedCalls(t, l2)
	if len(calls) != 1 || calls[0].Action != "attestDepositContract" {
		t.Fatalf("want one attestDepositContract, got %+v", calls)
	}
	var p struct {
		Gen         uint32 `json:"gen"`
		BlockHeight uint64 `json:"block_height"`
		Proof       string `json:"account_proof"`
	}
	if err := json.Unmarshal([]byte(calls[0].Payload), &p); err != nil || p.Gen != 1 || p.BlockHeight != 120 || p.Proof == "" {
		t.Fatalf("payload %s (%v): want gen 1 at the stored height 120 with a proof", calls[0].Payload, err)
	}
	l2.set("vsc1bridge", depositAttestedKey(1), "120")
	pass()
	if calls := submittedCalls(t, l2); len(calls) != 1 {
		t.Fatalf("attested again after the bridge recorded it: %d calls", len(calls))
	}
}
