package wasm_e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/common/params"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/hive_blocks"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	"vsc-node/modules/gql/gqlgen"
	"vsc-node/modules/gql/model"
	stateEngine "vsc-node/modules/state-processing"
)

// The 0.16.0 size bounds through a real contract, on the node's own call path
// and the dry run, on a chain whose elections say 0.14 at height 100 and 0.16
// at height 200. Below 0.16.0 nothing is bounded; from it one stored value
// over 1 MiB and one key over 1 KiB fail, and the dry run fails a call whose
// return value alone would exceed its contract's output bound.
func sizeBoundChain(t *testing.T) *test_utils.ContractTest {
	t.Helper()
	ct := newPriceChain(t)
	ct.IoWriteRcCost = 1 // the 0.15.0 price, so a 1 MiB value fits the payer's RC at both heights
	ct.Deposit(writePricePayer, 100_000_000, ledgerDb.AssetHbd)
	ct.RegisterContract("vscpricea", writePricePayer, compiledContract(t))
	ct.RegisterContract("vscpriceb", writePricePayer, compiledContract(t))
	ct.ElectionDb.ElectionsByHeight = map[uint64]elections.ElectionResult{
		100: electionAt(14),
		200: electionAt(16),
	}
	return ct
}

func chainCall(t *testing.T, ct *test_utils.ContractTest, height uint64, action, payload string, rcLimit uint64) stateEngine.TxResult {
	t.Helper()
	tx := stateEngine.TxVscCallContract{
		Self: stateEngine.TxSelf{
			TxId:          fmt.Sprintf("sizebound-%d-%s", height, action),
			BlockId:       "abcdef",
			BlockHeight:   height,
			Timestamp:     "2026-11-18T00:00:00",
			RequiredAuths: []string{writePricePayer},
		},
		NetId:      ct.StateEngine.SystemConfig().NetId(),
		ContractId: "vscpricea",
		Action:     action,
		Payload:    json.RawMessage(payload),
		RcLimit:    rcLimit,
	}
	return tx.ExecuteTx(ct.ChainEngine(), ct.LedgerSession, ct.RcSession, ct.CallSession, writePricePayer)
}

func TestSizeBound_StoredValueAndKeyOnTheChainPath(t *testing.T) {
	over := params.MAX_STATE_VALUE_BYTES + 1
	longKey := strings.Repeat("k", params.MAX_STATE_KEY_BYTES+1)
	for _, tc := range []struct {
		name    string
		height  uint64
		action  string
		payload string
		wantOk  bool
	}{
		{"0.14: a value over 1 MiB is stored", 100, "setBlob", fmt.Sprintf("v:%d", over), true},
		{"0.16: a value over 1 MiB fails", 200, "setBlob", fmt.Sprintf("v:%d", over), false},
		{"0.16: a value of exactly 1 MiB is stored", 200, "setBlob", fmt.Sprintf("w:%d", params.MAX_STATE_VALUE_BYTES), true},
		{"0.14: a value over 1 MiB stored one call down is stored", 100, "relayBlob", fmt.Sprintf("vscpriceb:v:%d", over), true},
		{"0.15: a value over 1 MiB stored one call down fails", 200, "relayBlob", fmt.Sprintf("vscpriceb:v:%d", over), false},
		{"0.14: a key over 1 KiB is stored", 100, "setString", longKey + ",x", true},
		{"0.16: a key over 1 KiB fails", 200, "setString", longKey + ",x", false},
		{"0.16: a key of exactly 1 KiB is stored", 200, "setString", strings.Repeat("j", params.MAX_STATE_KEY_BYTES) + ",x", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ct := sizeBoundChain(t)
			// Below 0.15 a new byte costs 19 RC: a 1 MiB value needs about 20 M.
			res := chainCall(t, ct, tc.height, tc.action, tc.payload, 30_000_000)
			if res.Success != tc.wantOk {
				t.Fatalf("success %v, want %v (%s)", res.Success, tc.wantOk, res.Ret)
			}
			if !tc.wantOk && !strings.Contains(res.Ret, "-byte limit") {
				t.Fatalf("refused with %v %q, want the limit named", res.Err, res.Ret)
			}
		})
	}
}

func TestSizeBound_DryRunFailsAReturnValueOverTheOutputBound(t *testing.T) {
	for _, tc := range []struct {
		name   string
		height uint64
		size   int
		wantOk bool
	}{
		{"0.14: a 1.1 MB return value passes", 100, 1_100_000, true},
		{"0.16: a 1.1 MB return value fails", 200, 1_100_000, false},
		{"0.16: a 900 KB return value passes", 200, 900_000, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ct := sizeBoundChain(t)
			resolver := &gqlgen.Resolver{
				HiveBlocks: &test_utils.MockHiveBlockDb{Blocks: []hive_blocks.HiveBlock{
					{BlockNumber: tc.height, BlockID: "abcdef", Timestamp: "2026-11-18T00:00:00"},
				}},
				StateEngine:    ct.StateEngine,
				Da:             ct.DataLayer,
				Contracts:      ct.ContractDb,
				ContractsState: ct.ContractStateDb,
				TssKeys:        ct.Tss.Keys,
				TssCommitments: ct.Tss.Commitments,
			}
			sim, err := resolver.Query().SimulateContractCalls(context.Background(), gqlgen.SimulateContractCallsInput{
				TxID:          "sizeboundsim",
				RequiredAuths: []string{writePricePayer},
				Calls: []gqlgen.SimulateContractCallInput{{
					ContractID: "vscpricea",
					Action:     "retBlob",
					Payload:    fmt.Sprint(tc.size),
					RcLimit:    model.Uint64(100_000),
				}},
			})
			if err != nil {
				t.Fatalf("simulateContractCalls: %v", err)
			}
			if len(sim) != 1 || sim[0].Success != tc.wantOk {
				t.Fatalf("dry run %+v, want success %v", sim, tc.wantOk)
			}
			if !tc.wantOk {
				if sim[0].Err == nil || *sim[0].Err != string(contracts.OUTPUT_TOO_LARGE) || sim[0].ErrMsg == nil || !strings.Contains(*sim[0].ErrMsg, "over the") {
					t.Fatalf("refused with %v %v, want output_too_large", sim[0].Err, sim[0].ErrMsg)
				}
			}
		})
	}
}

// The block's ledger record is bounded too: one call that makes enough ledger
// transfers to take it over 1 MiB fails (through the harness's dry-run-style
// check; the chain counts the block's other transactions as well). Each
// transfer costs 1 RC, so without the bound about 17,000 of them, some 17,000
// RC, would make a record no node outside the signing set could fetch.
func TestSizeBound_ManyLedgerTransfersInOneCall(t *testing.T) {
	for _, tc := range []struct {
		name   string
		bound  bool
		count  int
		wantOk bool
	}{
		{"no bound: 8,000 transfers pass", false, 8000, true},
		{"bound: 8,000 transfers fail", true, 8000, false},
		{"bound: 2,000 transfers pass", true, 2000, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ct := newPriceChain(t)
			ct.IoWriteRcCost = 1
			ct.StateSizeBound = tc.bound
			ct.Deposit(writePricePayer, 100_000_000, ledgerDb.AssetHbd)
			ct.RegisterContract("vscpricea", writePricePayer, compiledContract(t))
			res := ct.Call(stateEngine.TxVscCallContract{
				Self: stateEngine.TxSelf{
					TxId:          "manytransfers",
					BlockId:       "abcdef",
					Timestamp:     "2026-11-18T00:00:00",
					RequiredAuths: []string{writePricePayer},
				},
				ContractId: "vscpricea",
				Action:     "manyTransfers",
				Payload:    json.RawMessage(fmt.Sprint(tc.count)),
				RcLimit:    1_000_000,
				Intents:    []contracts.Intent{{Type: "transfer.allow", Args: map[string]string{"token": "hbd", "limit": "100.000"}}},
			})
			t.Logf("%d transfers: success %v, %d RC, %s %s", tc.count, res.Success, res.RcUsed, res.Err, res.ErrMsg)
			if res.Success != tc.wantOk {
				t.Fatalf("success %v, want %v", res.Success, tc.wantOk)
			}
			if !tc.wantOk && (res.Err != contracts.OUTPUT_TOO_LARGE || !strings.Contains(res.ErrMsg, "ledger record")) {
				t.Fatalf("refused with %s %q, want output_too_large for the ledger record", res.Err, res.ErrMsg)
			}
		})
	}
}
