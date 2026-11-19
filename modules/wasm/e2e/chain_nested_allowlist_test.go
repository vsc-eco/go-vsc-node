package wasm_e2e

import (
	"encoding/json"
	"testing"

	"vsc-node/modules/common/common_types"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc/elections"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	stateEngine "vsc-node/modules/state-processing"
)

// listingConfig is a network config whose TSS key allowlist is `list` at every
// height (the mocknet config lists nothing, which leaves create_key open).
type listingConfig struct {
	systemconfig.SystemConfig
	list []string
}

func (c listingConfig) TssKeyContractAllowlistAt(uint64) []string { return c.list }

// listingEngine is a chain engine on a listingConfig.
type listingEngine struct {
	common_types.StateEngine
	conf systemconfig.SystemConfig
}

func (e listingEngine) SystemConfig() systemconfig.SystemConfig { return e.conf }

// On the node's own call path (TxVscCallContract.ExecuteTx, which takes its
// rules from the chain-active version), a contract reached through
// contracts.call is held to the network's TSS key allowlist from consensus
// 0.16.0. The network lists vscpricea only; vscpricea relays to vscpriceb's
// createKey. Each case runs on a fresh chain, since a contract makes its key
// once.
func TestNestedAllowlist_ChainPathFollowsTheActiveVersion(t *testing.T) {
	for _, tc := range []struct {
		name        string
		consensus   uint64
		wantCreated bool
	}{
		{"0.15: the unlisted contract creates a key one call down", 15, true},
		{"0.16: the unlisted contract is refused one call down", 16, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ct := newPriceChain(t)
			ct.Deposit(writePricePayer, 100_000_000, ledgerDb.AssetHbd)
			ct.RegisterContract("vscpricea", writePricePayer, compiledContract(t))
			ct.RegisterContract("vscpriceb", writePricePayer, compiledContract(t))
			ct.ElectionDb.ElectionsByHeight = map[uint64]elections.ElectionResult{100: electionAt(tc.consensus)}
			engine := listingEngine{
				StateEngine: ct.ChainEngine(),
				conf:        listingConfig{SystemConfig: ct.StateEngine.SystemConfig(), list: []string{"vscpricea"}},
			}
			call := func(contract, action, payload string) (bool, string) {
				t.Helper()
				tx := stateEngine.TxVscCallContract{
					Self: stateEngine.TxSelf{
						TxId:          "chainallow-" + contract + "-" + action,
						BlockId:       "abcdef",
						BlockHeight:   150,
						Timestamp:     "2026-11-18T00:00:00",
						RequiredAuths: []string{writePricePayer},
					},
					NetId:      ct.StateEngine.SystemConfig().NetId(),
					ContractId: contract,
					Action:     action,
					Payload:    json.RawMessage(payload),
					RcLimit:    2_000_000,
				}
				res := tx.ExecuteTx(engine, ct.LedgerSession, ct.RcSession, ct.CallSession, writePricePayer)
				return res.Success, res.Ret
			}

			// The allowlist binds on this path at all: the unlisted contract
			// called directly is refused at both versions.
			if ok, ret := call("vscpriceb", "createKey", "x"); ok && ret == "created" {
				t.Fatalf("the unlisted contract created a key when called directly: %q", ret)
			}
			ok, ret := call("vscpricea", "relayCall", "vscpriceb,createKey,x")
			created := ok && ret == "created"
			t.Logf("one call down: success %v, %q", ok, ret)
			if created != tc.wantCreated {
				t.Fatalf("key created one call down: %v, want %v (%q)", created, tc.wantCreated, ret)
			}
			// The listed contract still creates its own key at both versions.
			if ok, ret := call("vscpricea", "createKey", "x"); !ok || ret != "created" {
				t.Fatalf("the listed contract could not create its key: success %v, %q", ok, ret)
			}
		})
	}
}
