package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"vsc-node/lib/btcvault"
)

// attestNeedL1 is the L1 an attest pass reads: eth_getProof at the stored
// height (codeHash, or with window set the reth proof-window refusal),
// eth_getCode at "finalized" (code), and the finalized block number.
type attestNeedL1 struct {
	codeHash, code atomic.Value
	finalized      atomic.Uint64
	window         atomic.Bool
	codeTags       atomic.Value // the block tags eth_getCode was asked for
}

func (f *attestNeedL1) rpc(t *testing.T) *ethRPC {
	t.Helper()
	f.codeHash.Store(emptyCodeHashHex)
	f.code.Store("0x")
	f.codeTags.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		res := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": v}) }
		switch req.Method {
		case "eth_getProof":
			if f.window.Load() {
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "error": map[string]any{"code": -32602,
					"message": "distance to target block exceeds maximum proof window"}})
				return
			}
			res(map[string]any{"codeHash": f.codeHash.Load().(string), "accountProof": []string{"0xc0"}, "nonce": "0x1", "balance": "0x0"})
		case "eth_getCode":
			if len(req.Params) > 1 {
				f.codeTags.Store(f.codeTags.Load().(string) + string(req.Params[1]))
			}
			res(f.code.Load().(string))
		case "eth_getBlockByNumber":
			res(map[string]any{"number": fmt.Sprintf("0x%x", f.finalized.Load())})
		default:
			t.Errorf("unexpected L1 call %s", req.Method)
		}
	}))
	t.Cleanup(srv.Close)
	return newEthRPC(srv.URL)
}

// needsReasons runs writeNeedsFile at the proven tip and returns its reasons.
func needsReasons(t *testing.T, sub *l2Submitter, cp *Checkpoint, cfg EVMBotConfig, tip uint64) (bool, []string) {
	t.Helper()
	needs := t.TempDir() + "/needs.json"
	t.Setenv("NEEDS_FILE", needs)
	writeNeedsFile(t.Context(), sub.gql, cp, cfg, tip, false)
	var nf struct {
		Want    bool     `json:"want_proof"`
		Reasons []string `json:"reasons"`
	}
	b, err := os.ReadFile(needs)
	if err != nil || json.Unmarshal(b, &nf) != nil {
		t.Fatalf("needs file unreadable: %v", err)
	}
	return nf.Want, nf.Reasons
}

// EVM-ATTEST-NEED-1 (testnet 11-16, 11-18): a pending generation's MagiDeposit
// whose code is at the finalized block F above the stored L1 height makes the
// bot want a proof until the stored height reaches F (so a quiet bridge does
// not hold the rotation until an unrelated deposit), and never otherwise: not
// while it is not deployed or not final, not once the height passed F (also
// when the attestation then fails), not for a generation attested elsewhere or
// no longer a candidate, and not from a stored height the endpoints refuse as
// past their proof window (a landed proof would be refused again).
func TestEVMAttestNeed1_ProofWantedUntilTheStoredHeightReachesTheDeploy(t *testing.T) {
	l2, sub := poolWorld(t)
	l2.set("vsc1bridge", "zkverifier", "vsc1verifier")
	l2.set("vsc1verifier", "h", "120")
	attestOK := true
	l2.sim = func(action string) map[string]any {
		if !attestOK {
			return map[string]any{"success": false, "err_msg": "generation has no key address yet", "rc_used": 1000}
		}
		return map[string]any{"success": true, "ret": "attested MagiDeposit", "rc_used": 3000}
	}
	var l1 attestNeedL1
	rpc := l1.rpc(t)
	cfg := EVMBotConfig{ContractID: "vsc1bridge", NetID: "vsc-devnet", RcLimit: 10000}
	view := vaultView{Registry: true, Gens: []vaultGen{
		{Gen: 1, Status: btcvault.VaultStatusActive, Address: "0x2b5ad5c4795c026514f8317c7a215e218dccd6cf"},
		{Gen: 2, Status: btcvault.VaultStatusPending, KeyID: "k2", Address: "0x6813eb9362372eef6200f3b1dbc3f819671cba69"},
	}}
	view.Active = &view.Gens[0]
	l2.set("vsc1bridge", depositAttestedKey(1), "100") // the active one is attested
	old := depositAttestEvery
	depositAttestEvery = 0
	t.Cleanup(func() { depositAttestEvery = old })
	cp := &Checkpoint{}
	pass := func(v vaultView) {
		depositAttest.Lock()
		depositAttest.at = time.Time{}
		depositAttest.Unlock()
		attestDepositContracts(t.Context(), rpc, sub.gql, sub, cp, cfg, v)
	}
	wanted := func(tip uint64) bool {
		want, reasons := needsReasons(t, sub, cp, cfg, tip)
		for _, r := range reasons {
			if strings.Contains(r, "EVM-ATTEST-NEED-1") {
				if !want {
					t.Fatalf("reason without want_proof: %v", reasons)
				}
				return true
			}
		}
		return false
	}

	l1.finalized.Store(150)
	pass(view) // not deployed (not final either): nothing wanted
	if cp.DepositAttestWantTip != 0 || wanted(120) {
		t.Fatalf("wants a proof for a MagiDeposit with no code at the finalized block (want tip %d)", cp.DepositAttestWantTip)
	}
	if tags := l1.codeTags.Load().(string); !strings.Contains(tags, `"finalized"`) || strings.Contains(tags, `"latest"`) {
		t.Fatalf("code read at %s, want the finalized block (a proof of the latest finalized checkpoint must cover it)", tags)
	}
	l1.code.Store("0x6080604052")
	pass(view) // final at 150, stored 120: wanted until the stored height reaches 150
	if cp.DepositAttestWantTip != 150 || !wanted(120) {
		t.Fatalf("a final MagiDeposit above the stored height does not make the bot want a proof (want tip %d)", cp.DepositAttestWantTip)
	}
	if calls := submittedCalls(t, l2); len(calls) != 0 {
		t.Fatalf("attested with no code at the stored height: %+v", calls)
	}
	if wanted(150) {
		t.Fatal("still wants a proof once the stored height reached the finalized deploy block")
	}
	// a pass that cannot read the stored height keeps the want
	l1.window.Store(true)
	pass(view)
	if cp.DepositAttestWantTip != 150 {
		t.Fatalf("an unreadable pass dropped the want (%d)", cp.DepositAttestWantTip)
	}
	// ...but never creates one: endpoints refusing the stored height cannot
	// serve a landed proof's height either (it is finalized, already old)
	cp.DepositAttestWantTip = 0
	pass(view)
	if cp.DepositAttestWantTip != 0 {
		t.Fatalf("a refused stored height made the bot want a proof (%d): it would buy one every batch window", cp.DepositAttestWantTip)
	}
	l1.window.Store(false)
	// attested elsewhere: dropped on the next pass
	pass(view)
	if cp.DepositAttestWantTip != 150 {
		t.Fatalf("want tip %d, want 150 again", cp.DepositAttestWantTip)
	}
	l2.set("vsc1bridge", depositAttestedKey(2), "150")
	pass(view)
	if cp.DepositAttestWantTip != 0 {
		t.Fatalf("a generation attested elsewhere is still wanted (%d)", cp.DepositAttestWantTip)
	}
	l2.set("vsc1bridge", depositAttestedKey(2), "")
	// no longer a candidate (superseded / cancelled): dropped
	pass(view)
	only := vaultView{Registry: true, Gens: view.Gens[:1]}
	only.Active = &only.Gens[0]
	pass(only)
	if cp.DepositAttestWantTip != 0 {
		t.Fatalf("a generation that is no longer a candidate is still wanted (%d)", cp.DepositAttestWantTip)
	}
	// the proof lands (stored 160, code there) but the attestation fails on L2:
	// no further proof is wanted
	l2.set("vsc1verifier", "h", "160")
	l1.codeHash.Store("0x" + strings.Repeat("ab", 32))
	l1.finalized.Store(170)
	attestOK = false
	pass(view)
	if cp.DepositAttestWantTip != 0 || wanted(160) {
		t.Fatalf("wants another proof after the height passed the deploy block (%d)", cp.DepositAttestWantTip)
	}
	attestOK = true
	pass(view)
	calls := submittedCalls(t, l2)
	if len(calls) != 1 || calls[0].Action != "attestDepositContract" || !strings.Contains(calls[0].Payload, `"gen":2`) {
		t.Fatalf("want one attestDepositContract for gen 2, got %+v", calls)
	}
}
