package wasm_e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/common/params"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/hive_blocks"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	"vsc-node/modules/gql/gqlgen"
	"vsc-node/modules/gql/model"
	stateEngine "vsc-node/modules/state-processing"
)

// newPriceChain is a fresh test chain whose data layer is closed when the test
// ends.
func newPriceChain(t *testing.T) *test_utils.ContractTest {
	t.Helper()
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	return &ct
}

const writePricePayer = "hive:someone"

// storeAt runs one call on a fresh chain at the given write price (0 = the
// harness default, the original price) and returns the RC it used. Three copies
// of the test contract are registered so a call can store from one or two
// contracts down.
func storeAt(t *testing.T, price int, action string, payload string) int64 {
	t.Helper()
	code := compiledContract(t)
	ct := newPriceChain(t)
	ct.IoWriteRcCost = price
	ct.Deposit(writePricePayer, 5_000_000, ledgerDb.AssetHbd)
	for _, id := range []string{"vscpricea", "vscpriceb", "vscpricec"} {
		ct.RegisterContract(id, writePricePayer, code)
	}
	res := ct.Call(stateEngine.TxVscCallContract{
		Self: stateEngine.TxSelf{
			TxId:          "writepricetx",
			BlockId:       "abcdef",
			Timestamp:     "2026-11-17T00:00:00",
			RequiredAuths: []string{writePricePayer},
		},
		ContractId: "vscpricea",
		Action:     action,
		Payload:    json.RawMessage(payload),
		RcLimit:    1_000_000,
	})
	if !res.Success {
		t.Fatalf("%s(%s) at write price %d failed: %s %s", action, payload, price, res.Err, res.ErrMsg)
	}
	return res.RcUsed
}

// A real contract storing N new bytes is charged exactly 18 RC a byte less at
// the 0.15.0 price than at the original one, and nothing else about the call
// changes. It holds for a contract called directly, for one reached through
// contracts.call, and for one reached two calls down: a nested call stores at
// the price of the transaction that started it. Without that, a swap paid out
// to a Bitcoin address (router -> pool -> mapping contract, where nearly all of
// the stored bytes are written by the innermost contract) would keep the old
// price.
func TestWritePrice_RealContractAtEveryCallDepth(t *testing.T) {
	const size = 5000
	cases := []struct {
		name    string
		action  string
		payload string
	}{
		{"called directly", "setBlob", fmt.Sprintf("k:%d", size)},
		{"one call down", "contractCall", fmt.Sprintf("vscpriceb,setBlob,k:%d", size)},
		{"two calls down", "contractCall", fmt.Sprintf("vscpriceb,relayBlob,vscpricec:k:%d", size)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			byDefault := storeAt(t, 0, tc.action, tc.payload)
			original := storeAt(t, params.WRITE_IO_GAS_RC_COST, tc.action, tc.payload)
			cut := storeAt(t, params.WRITE_IO_GAS_RC_COST_V0_15, tc.action, tc.payload)

			if byDefault != original {
				t.Errorf("the harness default charged %d RC, the original price %d: the default must be the original price",
					byDefault, original)
			}
			wantSaved := int64(size * (params.WRITE_IO_GAS_RC_COST - params.WRITE_IO_GAS_RC_COST_V0_15))
			if saved := original - cut; saved != wantSaved {
				t.Errorf("storing %d new bytes: %d RC at the original price, %d at the 0.15.0 price, a difference of %d; want %d (18 a byte)",
					size, original, cut, saved, wantSaved)
			}
			// What is left at the new price is the bytes themselves at 1 RC
			// plus the call's own work, far under the old charge.
			if cut < size || cut > size+2000 {
				t.Errorf("at the 0.15.0 price the call used %d RC, want %d for the bytes plus under 2,000 for the call itself", cut, size)
			}
			t.Logf("%s: %d RC -> %d RC", tc.name, original, cut)
		})
	}
}

// electionAt is an election whose committee runs the given consensus version.
func electionAt(consensus uint64) elections.ElectionResult {
	var r elections.ElectionResult
	r.ProtocolVersion = consensus
	return r
}

// The node's own call path, TxVscCallContract.ExecuteTx, charges the price of
// the consensus version that was active at the transaction's block height, and
// nothing else decides it.
//
// Three calls on one chain whose elections say 0.14 at height 100 and 0.15 at
// height 200, each storing the same number of new bytes:
//   - at 100 the original price,
//   - at 200 the 0.15.0 price,
//   - at 100 again the original price: a node that already knows of 0.15
//     re-executes older history at the price that history was charged.
//
// This is also the guard on the wiring: if the price option is ever dropped
// from ExecuteTx, the context falls back to the original price without any
// error, and the second call here fails.
func TestWritePrice_ChainPathChargesThePriceActiveAtTheTxHeight(t *testing.T) {
	const size = 5000
	ct := newPriceChain(t)
	ct.Deposit(writePricePayer, 5_000_000, ledgerDb.AssetHbd)
	ct.RegisterContract("vscpricea", writePricePayer, compiledContract(t))
	ct.ElectionDb.ElectionsByHeight = map[uint64]elections.ElectionResult{
		100: electionAt(14),
		200: electionAt(15),
	}

	run := func(height uint64, key string) int64 {
		t.Helper()
		tx := stateEngine.TxVscCallContract{
			Self: stateEngine.TxSelf{
				TxId:          "writepricetx-" + key,
				BlockId:       "abcdef",
				BlockHeight:   height,
				Timestamp:     "2026-11-17T00:00:00",
				RequiredAuths: []string{writePricePayer},
			},
			NetId:      ct.StateEngine.SystemConfig().NetId(),
			ContractId: "vscpricea",
			Action:     "setBlob",
			Payload:    json.RawMessage(fmt.Sprintf("%s:%d", key, size)),
			RcLimit:    1_000_000,
		}
		res := tx.ExecuteTx(ct.ChainEngine(), ct.LedgerSession, ct.RcSession, ct.CallSession, writePricePayer)
		if !res.Success {
			t.Fatalf("ExecuteTx at height %d failed: %s", height, res.Ret)
		}
		return res.RcUsed
	}

	below := run(100, "k1")
	above := run(200, "k2")
	replayed := run(100, "k3")

	// Each call stores `size` new bytes under a new two-byte key: the key is
	// read, the value is new, and the call's own work rounds up to one more RC.
	wantBelow := int64(2 + size*params.WRITE_IO_GAS_RC_COST + 1)
	wantAbove := int64(2 + size*params.WRITE_IO_GAS_RC_COST_V0_15 + 1)
	if below != wantBelow {
		t.Errorf("below 0.15.0 (height 100): %d RC, want %d (19 a new byte)", below, wantBelow)
	}
	if above != wantAbove {
		t.Errorf("at 0.15.0 (height 200): %d RC, want %d (1 a new byte). If this reads %d, ExecuteTx no longer passes the write price to the execution context",
			above, wantAbove, wantBelow)
	}
	if replayed != wantBelow {
		t.Errorf("history below 0.15.0 re-executed after the switch: %d RC, want %d (the price it was charged)", replayed, wantBelow)
	}
}

// A dry run charges what the chain charges at the same height, before and
// after the 0.15.0 write price.
//
// Wallets and bots size rc_limit from simulateContractCalls. A preview left on
// the original price after the switch would make every caller declare (and
// need to hold) 19 times too much for the bytes a call stores; a preview that
// took the new price before the chain did would size limits the chain then
// overruns, and a call that runs out is charged its whole limit.
//
// The real resolver and the real TxVscCallContract.ExecuteTx run the same call
// against the same chain, whose elections say 0.14 at height 100 and 0.15 at
// height 200.
func TestWritePrice_DryRunChargesWhatTheChainCharges(t *testing.T) {
	const size = 2000 // a dry run's rc_limit stops at 100,000

	cases := []struct {
		name      string
		height    uint64
		wantPrice int
	}{
		{"0.14.0: the original price", 100, params.WRITE_IO_GAS_RC_COST},
		{"0.15.0: the cut price", 200, params.WRITE_IO_GAS_RC_COST_V0_15},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ct := newPriceChain(t)
			ct.Deposit(writePricePayer, 5_000_000, ledgerDb.AssetHbd)
			ct.RegisterContract("vscpricea", writePricePayer, compiledContract(t))
			ct.ElectionDb.ElectionsByHeight = map[uint64]elections.ElectionResult{
				100: electionAt(14),
				200: electionAt(15),
			}
			payload := fmt.Sprintf("k:%d", size)

			// The preview first: it must not leave anything behind for the
			// chain call below to find.
			resolver := &gqlgen.Resolver{
				HiveBlocks: &test_utils.MockHiveBlockDb{Blocks: []hive_blocks.HiveBlock{
					{BlockNumber: tc.height, BlockID: "abcdef", Timestamp: "2026-11-17T00:00:00"},
				}},
				StateEngine:    ct.StateEngine,
				Da:             ct.DataLayer,
				Contracts:      ct.ContractDb,
				ContractsState: ct.ContractStateDb,
				TssKeys:        ct.Tss.Keys,
				TssCommitments: ct.Tss.Commitments,
			}
			sim, err := resolver.Query().SimulateContractCalls(context.Background(), gqlgen.SimulateContractCallsInput{
				TxID:          "writepricesim",
				RequiredAuths: []string{writePricePayer},
				Calls: []gqlgen.SimulateContractCallInput{{
					ContractID: "vscpricea",
					Action:     "setBlob",
					Payload:    payload,
					RcLimit:    model.Uint64(100_000),
				}},
			})
			if err != nil {
				t.Fatalf("simulateContractCalls: %v", err)
			}
			if len(sim) != 1 || !sim[0].Success {
				t.Fatalf("simulateContractCalls did not succeed: %+v", sim)
			}
			simulated := int64(sim[0].RcUsed)

			tx := stateEngine.TxVscCallContract{
				Self: stateEngine.TxSelf{
					TxId:          "writepricetx",
					BlockId:       "abcdef",
					BlockHeight:   tc.height,
					Timestamp:     "2026-11-17T00:00:00",
					RequiredAuths: []string{writePricePayer},
				},
				NetId:      ct.StateEngine.SystemConfig().NetId(),
				ContractId: "vscpricea",
				Action:     "setBlob",
				Payload:    json.RawMessage(payload),
				RcLimit:    100_000,
			}
			res := tx.ExecuteTx(ct.ChainEngine(), ct.LedgerSession, ct.RcSession, ct.CallSession, writePricePayer)
			if !res.Success {
				t.Fatalf("ExecuteTx failed: %s", res.Ret)
			}

			// The one-byte key is read, the value is new, and the call's own
			// work rounds up to one more RC.
			want := int64(1 + size*tc.wantPrice + 1)
			if res.RcUsed != want {
				t.Errorf("the chain charged %d RC, want %d (%d a new byte)", res.RcUsed, want, tc.wantPrice)
			}
			if simulated != res.RcUsed {
				t.Errorf("the dry run reported %d RC, the chain charged %d: a preview must charge the chain's price", simulated, res.RcUsed)
			}
		})
	}
}

// The chain path reads the election in force at a height, not only one keyed
// at exactly that height: a call between two elections, and one after the
// last, is charged the price of the election before it.
func TestWritePrice_ChainPathBetweenElections(t *testing.T) {
	const size = 1000
	ct := newPriceChain(t)
	ct.Deposit(writePricePayer, 5_000_000, ledgerDb.AssetHbd)
	ct.RegisterContract("vscpricea", writePricePayer, compiledContract(t))
	ct.ElectionDb.ElectionsByHeight = map[uint64]elections.ElectionResult{
		100: electionAt(14),
		200: electionAt(15),
	}
	for i, tc := range []struct {
		height    uint64
		wantPrice int
	}{
		{150, params.WRITE_IO_GAS_RC_COST},
		{199, params.WRITE_IO_GAS_RC_COST},
		{201, params.WRITE_IO_GAS_RC_COST_V0_15},
		{5000, params.WRITE_IO_GAS_RC_COST_V0_15},
	} {
		tx := stateEngine.TxVscCallContract{
			Self: stateEngine.TxSelf{
				TxId:          fmt.Sprintf("between-%d", i),
				BlockId:       "abcdef",
				BlockHeight:   tc.height,
				Timestamp:     "2026-11-17T00:00:00",
				RequiredAuths: []string{writePricePayer},
			},
			NetId:      ct.StateEngine.SystemConfig().NetId(),
			ContractId: "vscpricea",
			Action:     "setBlob",
			Payload:    json.RawMessage(fmt.Sprintf("b%d:%d", i, size)),
			RcLimit:    1_000_000,
		}
		res := tx.ExecuteTx(ct.ChainEngine(), ct.LedgerSession, ct.RcSession, ct.CallSession, writePricePayer)
		if !res.Success {
			t.Fatalf("ExecuteTx at height %d failed: %s", tc.height, res.Ret)
		}
		if want := int64(2 + size*tc.wantPrice + 1); res.RcUsed != want {
			t.Errorf("height %d: %d RC, want %d (%d a new byte)", tc.height, res.RcUsed, want, tc.wantPrice)
		}
	}
}

// A call that runs out of RC. A storage operation is charged after it ran, so
// the one that takes a call past its rc_limit is counted in full. Up to
// consensus 0.14.0 the payer was charged all of it: with rc_limit 1,000 and one
// 1 MB log, 1,000,036 RC. From 0.15.0 the charge stops at the limit.
//
// The node's own call path and the dry run, on a chain whose elections say 0.14
// at height 100 and 0.15 at height 200.
func TestRcCharge_ACallThatRunsOutIsChargedItsLimitFrom015(t *testing.T) {
	const limit = 1000
	cases := []struct {
		name    string
		action  string
		payload string
		// what the storage operation alone costs at 0.14, far over the limit
		costAt014 int64
	}{
		{"one 1 MB log", "logBlob", "1000000", 1_000_000},
		{"one 4,000 byte value", "setBlob", "big:4000", 4000 * params.WRITE_IO_GAS_RC_COST},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, at := range []struct {
				height uint64
				capped bool
			}{{100, false}, {200, true}} {
				ct := newPriceChain(t)
				ct.Deposit(writePricePayer, 50_000_000, ledgerDb.AssetHbd)
				ct.RegisterContract("vscpricea", writePricePayer, compiledContract(t))
				ct.ElectionDb.ElectionsByHeight = map[uint64]elections.ElectionResult{
					100: electionAt(14),
					200: electionAt(15),
				}

				resolver := &gqlgen.Resolver{
					HiveBlocks: &test_utils.MockHiveBlockDb{Blocks: []hive_blocks.HiveBlock{
						{BlockNumber: at.height, BlockID: "abcdef", Timestamp: "2026-11-17T00:00:00"},
					}},
					StateEngine:    ct.StateEngine,
					Da:             ct.DataLayer,
					Contracts:      ct.ContractDb,
					ContractsState: ct.ContractStateDb,
					TssKeys:        ct.Tss.Keys,
					TssCommitments: ct.Tss.Commitments,
				}
				sim, err := resolver.Query().SimulateContractCalls(context.Background(), gqlgen.SimulateContractCallsInput{
					TxID:          "runoutsim",
					RequiredAuths: []string{writePricePayer},
					Calls: []gqlgen.SimulateContractCallInput{{
						ContractID: "vscpricea", Action: tc.action, Payload: tc.payload, RcLimit: model.Uint64(limit),
					}},
				})
				if err != nil || len(sim) != 1 {
					t.Fatalf("height %d: simulateContractCalls: %v %+v", at.height, err, sim)
				}

				tx := stateEngine.TxVscCallContract{
					Self: stateEngine.TxSelf{
						TxId: "runouttx", BlockId: "abcdef", BlockHeight: at.height,
						Timestamp: "2026-11-17T00:00:00", RequiredAuths: []string{writePricePayer},
					},
					NetId:      ct.StateEngine.SystemConfig().NetId(),
					ContractId: "vscpricea", Action: tc.action, Payload: json.RawMessage(tc.payload), RcLimit: limit,
				}
				res := tx.ExecuteTx(ct.ChainEngine(), ct.LedgerSession, ct.RcSession, ct.CallSession, writePricePayer)

				if res.Success || sim[0].Success {
					t.Fatalf("height %d: the call must run out under rc_limit %d (chain success=%v, dry run success=%v)", at.height, limit, res.Success, sim[0].Success)
				}
				if at.capped {
					if res.RcUsed != limit {
						t.Errorf("height %d (0.15): charged %d RC, want the limit %d", at.height, res.RcUsed, limit)
					}
				} else if res.RcUsed < tc.costAt014 {
					t.Errorf("height %d (0.14): charged %d RC, want the whole operation, at least %d, as history was charged", at.height, res.RcUsed, tc.costAt014)
				}
				if int64(sim[0].RcUsed) != res.RcUsed {
					t.Errorf("height %d: the dry run reports %d RC, the chain charged %d", at.height, int64(sim[0].RcUsed), res.RcUsed)
				}
				t.Logf("height %d: rc_limit %d, charged %d RC", at.height, limit, res.RcUsed)
			}
		})
	}
}

// The cap changes nothing for a call that finishes: at 0.15.0 a call given
// exactly what it costs passes and is charged exactly that.
func TestRcCharge_ACallThatFinishesIsChargedWhatItUsed(t *testing.T) {
	const size = 4000
	ct := newPriceChain(t)
	ct.Deposit(writePricePayer, 5_000_000, ledgerDb.AssetHbd)
	ct.RegisterContract("vscpricea", writePricePayer, compiledContract(t))
	ct.ElectionDb.ElectionsByHeight = map[uint64]elections.ElectionResult{200: electionAt(15)}
	run := func(key string, limit uint64) stateEngine.TxResult {
		tx := stateEngine.TxVscCallContract{
			Self: stateEngine.TxSelf{
				TxId: "exact-" + key, BlockId: "abcdef", BlockHeight: 200,
				Timestamp: "2026-11-17T00:00:00", RequiredAuths: []string{writePricePayer},
			},
			NetId:      ct.StateEngine.SystemConfig().NetId(),
			ContractId: "vscpricea", Action: "setBlob", Payload: json.RawMessage(fmt.Sprintf("%s:%d", key, size)), RcLimit: limit,
		}
		return tx.ExecuteTx(ct.ChainEngine(), ct.LedgerSession, ct.RcSession, ct.CallSession, writePricePayer)
	}
	roomy := run("e1", 1_000_000)
	if !roomy.Success {
		t.Fatalf("the call failed with room to spare: %s", roomy.Ret)
	}
	exact := run("e2", uint64(roomy.RcUsed))
	if !exact.Success || exact.RcUsed != roomy.RcUsed {
		t.Errorf("given exactly its cost (%d RC): success=%v charged %d", roomy.RcUsed, exact.Success, exact.RcUsed)
	}
	short := run("e3", uint64(roomy.RcUsed-1))
	if short.Success || short.RcUsed != roomy.RcUsed-1 {
		t.Errorf("given one RC less (%d): success=%v charged %d, want a failure charged the limit", roomy.RcUsed-1, short.Success, short.RcUsed)
	}
}

// The contract test harness follows the same rule when asked to
// (ContractTest.RcChargeCapped), and keeps the original charge otherwise, so a
// contract suite can measure a failed call as the chain will charge it.
func TestRcCharge_HarnessCapsWhenAsked(t *testing.T) {
	run := func(capped bool) int64 {
		ct := newPriceChain(t)
		ct.RcChargeCapped = capped
		ct.Deposit(writePricePayer, 50_000_000, ledgerDb.AssetHbd)
		ct.RegisterContract("vscpricea", writePricePayer, compiledContract(t))
		res := ct.Call(stateEngine.TxVscCallContract{
			Self:       stateEngine.TxSelf{TxId: "harnesscap", BlockId: "abcdef", Timestamp: "2026-11-17T00:00:00", RequiredAuths: []string{writePricePayer}},
			ContractId: "vscpricea", Action: "logBlob", Payload: json.RawMessage("1000000"), RcLimit: 1000,
		})
		if res.Success {
			t.Fatalf("the call must run out under rc_limit 1000")
		}
		return res.RcUsed
	}
	if got := run(false); got < 1_000_000 {
		t.Errorf("default harness: charged %d RC, want the whole operation (the original charge)", got)
	}
	if got := run(true); got != 1000 {
		t.Errorf("RcChargeCapped: charged %d RC, want the limit 1000", got)
	}
}
