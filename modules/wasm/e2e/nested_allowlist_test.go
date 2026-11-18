package wasm_e2e

import (
	"encoding/json"
	"testing"

	ledgerDb "vsc-node/modules/db/vsc/ledger"
	stateEngine "vsc-node/modules/state-processing"
)

// The TSS key-creation allowlist (only listed contracts may create TSS keys)
// applies to a contract reached through contracts.call from consensus 0.16.0;
// below it the nested context carried no allowlist. Here vscpricea is listed,
// vscpriceb is not, and vscpricea calls vscpriceb's createKey.
func TestNestedCallIsHeldToTheTssKeyAllowlist(t *testing.T) {
	for _, tc := range []struct {
		name         string
		nested       bool
		wantKeyMade  bool
		directCallOk bool
	}{
		{"before 0.16.0: the unlisted contract creates a key one call down", false, true, false},
		{"from 0.16.0: the unlisted contract is refused one call down too", true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ct := newPriceChain(t)
			ct.Deposit(writePricePayer, 100_000_000, ledgerDb.AssetHbd)
			ct.RegisterContract("vscpricea", writePricePayer, compiledContract(t))
			ct.RegisterContract("vscpriceb", writePricePayer, compiledContract(t))
			ct.TssKeyAllowlist = []string{"vscpricea"}
			ct.TssKeyAllowlistNested = tc.nested
			call := func(contract, action, payload string) (bool, string) {
				res := ct.Call(stateEngine.TxVscCallContract{
					Self:       stateEngine.TxSelf{TxId: "allowlist-" + action, BlockId: "abcdef", Timestamp: "2026-11-18T00:00:00", RequiredAuths: []string{writePricePayer}},
					ContractId: contract,
					Action:     action,
					Payload:    json.RawMessage(payload),
					RcLimit:    2_000_000,
				})
				return res.Success, res.Ret + res.ErrMsg
			}
			// Called directly, the unlisted contract is refused either way.
			if ok, ret := call("vscpriceb", "createKey", "x"); ok && ret == "created" {
				t.Fatalf("the unlisted contract created a key when called directly: %q", ret)
			}
			ok, ret := call("vscpricea", "relayCall", "vscpriceb,createKey,x")
			made := ok && ret == "created"
			t.Logf("one call down: success %v, %q", ok, ret)
			if made != tc.wantKeyMade {
				t.Fatalf("key created one call down: %v, want %v (%q)", made, tc.wantKeyMade, ret)
			}
		})
	}
}
