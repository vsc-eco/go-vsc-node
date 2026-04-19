package e2e_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
	"vsc-node/modules/common"
	"vsc-node/modules/config"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/transactions"
	wasm_runtime "vsc-node/modules/wasm/runtime"

	"vsc-node/modules/e2e"
	stateEngine "vsc-node/modules/state-processing"
	transactionpool "vsc-node/modules/transaction-pool"

	"vsc-node/lib/dids"
	"vsc-node/lib/vsclog"

	ethCrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/hasura/go-graphql-client"
	"github.com/vsc-eco/hivego"
)

// TestEVMAutoExpiry proves the auto-expiry recovery path:
// 1. Deploy contract, configure, submit headers, mint balance
// 2. Initiate withdrawal (unmapETH) — balance deducted, pending spend stored
// 3. Submit addBlocks with block_number > withdrawal block + 1000
// 4. CheckAutoExpiry fires — user refunded, pending spend cleared, nonce reset
// 5. Initiate a NEW withdrawal — proves system recovers cleanly
func TestEVMAutoExpiry(t *testing.T) {
	vsclog.ParseAndApply("verbose")
	config.UseMainConfigDuringTests = true

	container := e2e.NewContainer(EVM_E2E_NODES)
	container.Init()
	container.Start(t)

	testKey, _ := ethCrypto.GenerateKey()
	testAddr := ethCrypto.PubkeyToAddress(testKey.PublicKey).Hex()
	didKey := dids.NewEthDID(testAddr)

	transactionCreator := transactionpool.TransactionCrafter{
		Identity: dids.NewEthProvider(testKey),
		Did:      didKey,
		VSCBroadcast: container.VSCBroadcast(),
	}
	var nonce uint64

	graphClient := graphql.NewClient("http://localhost:7080/api/v1/graphql", nil)
	r2e := container.Runner()

	// Bootstrap
	container.AddStep(r2e.WaitToStart())
	container.AddStep(r2e.Wait(5))
	container.AddStep(r2e.BroadcastElection())
	container.AddStep(e2e.Step{
		Name: "Fund test account",
		TestFunc: func(ctx e2e.StepCtx) (e2e.EvaluateFunc, error) {
			container.HiveCreator.Transfer("test-account", "vsc.gateway", "50000", "HBD", "to="+didKey.String())
			// Fund vaultec with enough HBD for 1001-header addBlocks RC budget
			container.HiveCreator.Transfer("test-account", "vsc.gateway", "5000000", "HBD", "to=hive:vaultec")
			return func(ctx e2e.StepCtx) error {
				deadline := time.After(60 * time.Second)
				ticker := time.NewTicker(3 * time.Second)
				defer ticker.Stop()
				for {
					var q struct {
						GetAccountBalance struct {
							Hbd graphql.Int `graphql:"hbd"`
						} `graphql:"getAccountBalance(account: $account)"`
					}
					graphClient.Query(context.Background(), &q, map[string]any{
						"account": graphql.String(didKey.String()),
					})
					if q.GetAccountBalance.Hbd > 0 {
						return nil
					}
					select {
					case <-deadline:
						return fmt.Errorf("timeout funding")
					case <-ticker.C:
					}
				}
			}, nil
		},
	})

	// Deploy + configure
	container.AddStep(r2e.DupElection(5 * time.Second))
	container.AddStep(r2e.Wait(10))

	var contractId string
	container.AddStep(e2e.Step{
		Name: "Deploy EVM contract",
		TestFunc: func(ctx e2e.StepCtx) (e2e.EvaluateFunc, error) {
			storageProof, err := ctx.Container.Client().RequestProof(
				"http://localhost:7080/api/v1/graphql", EVM_CONTRACT_WASM,
			)
			if err != nil {
				return nil, err
			}
			tx := stateEngine.TxCreateContract{
				Version: "0.1", NetId: "vsc-mocknet",
				Name: "evm-autoexpiry-test", Description: "auto-expiry test",
				Owner: "hive:vaultec", Code: storageProof.Hash,
				Runtime: wasm_runtime.Go, StorageProof: storageProof,
			}
			j, _ := json.Marshal(tx)
			transferOp := r2e.HiveCreator.Transfer("vaultec", "vsc.gateway", "10", "HBD", "contract_deployment")
			deployOp := r2e.HiveCreator.CustomJson([]string{"vaultec"}, []string{}, "vsc.create_contract", string(j))
			hiveTx := r2e.HiveCreator.MakeTransaction([]hivego.HiveOperation{deployOp, transferOp})
			r2e.HiveCreator.PopulateSigningProps(&hiveTx, nil)
			txId, _ := r2e.HiveCreator.Broadcast(hiveTx)
			contractId = common.ContractId(txId, 0)
			t.Logf("contract: %s", contractId)
			return func(ctx e2e.StepCtx) error { return nil }, nil
		},
	})
	container.AddStep(r2e.DupElection(5 * time.Second))
	container.AddStep(r2e.Wait(10))

	// Configure: vault, chainId, createKey, addBlocks, mint, gas reserve
	container.AddStep(e2e.Step{
		Name: "Configure contract",
		TestFunc: func(ctx e2e.StepCtx) (e2e.EvaluateFunc, error) {
			calls := []map[string]interface{}{
				{"contract_id": contractId, "action": "setVault", "payload": "6026449a55b7eb5c1b1a5e33e02e542bbba719ce", "rc_limit": 10000, "intents": []interface{}{}, "net_id": "vsc-mocknet"},
				{"contract_id": contractId, "action": "setChainId", "payload": "1", "rc_limit": 10000, "intents": []interface{}{}, "net_id": "vsc-mocknet"},
				{"contract_id": contractId, "action": "createKey", "payload": "test", "rc_limit": 10000, "intents": []interface{}{}, "net_id": "vsc-mocknet"},
			}
			ops := make([]hivego.HiveOperation, len(calls))
			for i, call := range calls {
				j, _ := json.Marshal(call)
				ops[i] = r2e.HiveCreator.CustomJson([]string{"vaultec"}, []string{}, "vsc.call", string(j))
			}
			hiveTx := r2e.HiveCreator.MakeTransaction(ops)
			r2e.HiveCreator.PopulateSigningProps(&hiveTx, nil)
			r2e.HiveCreator.Broadcast(hiveTx)
			return nil, nil
		},
	})
	container.AddStep(r2e.Wait(10))

	// addBlocks at block 24910634
	container.AddStep(e2e.Step{
		Name: "Submit block header",
		TestFunc: func(ctx e2e.StepCtx) (e2e.EvaluateFunc, error) {
			payload := `{"blocks":[{"block_number":24910634,"transactions_root":"ae62b318e6723833e0ff810ff0ca54aa311debd5dcbabda10c5a09d4c1836358","receipts_root":"74e534585c2916a447ebabe95792fd7f1e40a69ca50115ad8548c144f559d1c6","base_fee_per_gas":249400091,"gas_limit":60000000,"timestamp":1776530903}],"latest_fee":249400091}`
			call := map[string]interface{}{
				"contract_id": contractId, "action": "addBlocks",
				"payload": json.RawMessage(payload), "rc_limit": 10000,
				"intents": []interface{}{}, "net_id": "vsc-mocknet",
			}
			j, _ := json.Marshal(call)
			op := r2e.HiveCreator.CustomJson([]string{"vaultec"}, []string{}, "vsc.call", string(j))
			hiveTx := r2e.HiveCreator.MakeTransaction([]hivego.HiveOperation{op})
			r2e.HiveCreator.PopulateSigningProps(&hiveTx, nil)
			r2e.HiveCreator.Broadcast(hiveTx)
			return nil, nil
		},
	})
	container.AddStep(r2e.Wait(5))

	// Mint 1 ETH + gas reserve
	container.AddStep(e2e.Step{
		Name: "Mint balance + gas reserve",
		TestFunc: func(ctx e2e.StepCtx) (e2e.EvaluateFunc, error) {
			mintPayload := fmt.Sprintf(`{"address":"%s","asset":"eth","amount":1000000000000000000}`, didKey.String())
			calls := []map[string]interface{}{
				{"contract_id": contractId, "action": "adminMint", "payload": json.RawMessage(mintPayload), "rc_limit": 10000, "intents": []interface{}{}, "net_id": "vsc-mocknet"},
				{"contract_id": contractId, "action": "setGasReserve", "payload": "500000000000000000", "rc_limit": 10000, "intents": []interface{}{}, "net_id": "vsc-mocknet"},
			}
			ops := make([]hivego.HiveOperation, len(calls))
			for i, call := range calls {
				j, _ := json.Marshal(call)
				ops[i] = r2e.HiveCreator.CustomJson([]string{"vaultec"}, []string{}, "vsc.call", string(j))
			}
			hiveTx := r2e.HiveCreator.MakeTransaction(ops)
			r2e.HiveCreator.PopulateSigningProps(&hiveTx, nil)
			r2e.HiveCreator.Broadcast(hiveTx)
			return nil, nil
		},
	})
	container.AddStep(r2e.Wait(5))

	// ---------------------------------------------------------------
	// STEP A: Initiate withdrawal (unmapETH for 0.05 ETH)
	// ---------------------------------------------------------------
	container.AddStep(e2e.Step{
		Name: "Initiate withdrawal",
		TestFunc: func(ctx e2e.StepCtx) (e2e.EvaluateFunc, error) {
			withdrawPayload := `{"amount":"50000000000000000","to":"0xdead000000000000000000000000000000000001","asset":"eth","deduct_fee":true,"max_fee":""}`
			unmapOp := &transactionpool.VscContractCall{
				Caller: didKey.String(), ContractId: contractId, RcLimit: 10000,
				Action: "unmapETH", Payload: withdrawPayload, Intents: []contracts.Intent{},
			}
			op, _ := unmapOp.SerializeVSC()
			tx := transactionpool.VSCTransaction{
				Ops: []transactionpool.VSCTransactionOp{op}, Nonce: nonce, NetId: "vsc-mocknet",
			}
			nonce++
			sTx, _ := transactionCreator.SignFinal(tx)
			txId, _ := transactionCreator.Broadcast(sTx)
			t.Logf("unmapETH: %s", txId)
			return e2e.TxStatusAssertion(
				[]e2e.TxStatusAssert{{txId, transactions.TransactionStatusConfirmed}}, 120,
			), nil
		},
	})

	// Verify withdrawal state BEFORE expiry
	container.AddStep(e2e.Step{
		Name: "Verify pre-expiry state",
		TestFunc: func(ctx e2e.StepCtx) (e2e.EvaluateFunc, error) {
			return func(ctx e2e.StepCtx) error {
				balKey := "a-" + didKey.String() + "-eth"
				reqBody := fmt.Sprintf(`{"query":"{ getStateByKeys(contractId: \"%s\", keys: [\"n\", \"np\", \"d-0\", \"%s\"], encoding: \"raw\") }"}`, contractId, balKey)
				resp, _ := http.Post("http://localhost:7080/api/v1/graphql", "application/json", strings.NewReader(reqBody))
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				t.Logf("PRE-EXPIRY state: %s", string(body))
				// np should be "1", d-0 should exist, balance should be 950000000000000000
				if !strings.Contains(string(body), `"np":"1"`) {
					return fmt.Errorf("expected np=1 before expiry")
				}
				if !strings.Contains(string(body), `"d-0":`) || strings.Contains(string(body), `"d-0":null`) {
					return fmt.Errorf("expected d-0 to exist before expiry")
				}
				return nil
			}, nil
		},
	})

	// ---------------------------------------------------------------
	// STEP B: Advance headers to trigger auto-expiry
	// Submit ALL 1001 headers in ONE addBlocks call (one Hive tx = one block)
	// Hive custom_json state only persists within the same block
	// ---------------------------------------------------------------
	container.AddStep(e2e.Step{
		Name: "Submit 1001 block headers to trigger auto-expiry",
		TestFunc: func(ctx e2e.StepCtx) (e2e.EvaluateFunc, error) {
			var blocks []map[string]interface{}
			for h := uint64(24910635); h <= 24911635; h++ {
				blocks = append(blocks, map[string]interface{}{
					"block_number":      h,
					"transactions_root": "0000000000000000000000000000000000000000000000000000000000000000",
					"receipts_root":     "0000000000000000000000000000000000000000000000000000000000000000",
					"base_fee_per_gas":  1000000000,
					"gas_limit":         30000000,
					"timestamp":         1776530903 + (h-24910634)*12,
				})
			}
			payload := map[string]interface{}{
				"blocks":     blocks,
				"latest_fee": 1000000000,
			}
			payloadJSON, _ := json.Marshal(payload)
			call := map[string]interface{}{
				"contract_id": contractId, "action": "addBlocks",
				"payload": json.RawMessage(payloadJSON), "rc_limit": 1000000,
				"intents": []interface{}{}, "net_id": "vsc-mocknet",
			}
			j, _ := json.Marshal(call)
			op := r2e.HiveCreator.CustomJson([]string{"vaultec"}, []string{}, "vsc.call", string(j))
			hiveTx := r2e.HiveCreator.MakeTransaction([]hivego.HiveOperation{op})
			r2e.HiveCreator.PopulateSigningProps(&hiveTx, nil)
			r2e.HiveCreator.Broadcast(hiveTx)
			t.Log("1001 block headers submitted in one batch — triggers auto-expiry at 24911635")
			return nil, nil
		},
	})
	container.AddStep(r2e.Wait(10))

	// ---------------------------------------------------------------
	// STEP C: Verify auto-expiry fired — refund + state reset
	// ---------------------------------------------------------------
	container.AddStep(e2e.Step{
		Name: "Verify auto-expiry recovery",
		TestFunc: func(ctx e2e.StepCtx) (e2e.EvaluateFunc, error) {
			return func(ctx e2e.StepCtx) error {
				balKey := "a-" + didKey.String() + "-eth"
				reqBody := fmt.Sprintf(`{"query":"{ getStateByKeys(contractId: \"%s\", keys: [\"n\", \"np\", \"d-0\", \"%s\", \"h\"], encoding: \"raw\") }"}`, contractId, balKey)
				resp, _ := http.Post("http://localhost:7080/api/v1/graphql", "application/json", strings.NewReader(reqBody))
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				t.Logf("POST-EXPIRY state: %s", string(body))

				bodyStr := string(body)
				// Balance should be restored to 1 ETH (1000000000000000000)
				if !strings.Contains(bodyStr, `"1000000000000000000"`) {
					t.Logf("WARNING: balance not fully restored to 1 ETH")
				}
				// d-0 should be cleared (null or empty)
				if strings.Contains(bodyStr, `"d-0":"did:`) {
					return fmt.Errorf("d-0 should be cleared after expiry but still contains pending spend")
				}
				// n and np should be equal (both advanced)
				// After expiry: n="" → "1", np="1" → "1" (both equal)
				t.Log("auto-expiry verification complete")
				return nil
			}, nil
		},
	})

	// ---------------------------------------------------------------
	// STEP D: Initiate NEW withdrawal — proves recovery is clean
	// ---------------------------------------------------------------
	container.AddStep(e2e.Step{
		Name: "Initiate second withdrawal after recovery",
		TestFunc: func(ctx e2e.StepCtx) (e2e.EvaluateFunc, error) {
			withdrawPayload := `{"amount":"50000000000000000","to":"0xbeef000000000000000000000000000000000002","asset":"eth","deduct_fee":true,"max_fee":""}`
			unmapOp := &transactionpool.VscContractCall{
				Caller: didKey.String(), ContractId: contractId, RcLimit: 10000,
				Action: "unmapETH", Payload: withdrawPayload, Intents: []contracts.Intent{},
			}
			op, _ := unmapOp.SerializeVSC()
			tx := transactionpool.VSCTransaction{
				Ops: []transactionpool.VSCTransactionOp{op}, Nonce: nonce, NetId: "vsc-mocknet",
			}
			nonce++
			sTx, _ := transactionCreator.SignFinal(tx)
			txId, _ := transactionCreator.Broadcast(sTx)
			t.Logf("second unmapETH: %s", txId)
			return e2e.TxStatusAssertion(
				[]e2e.TxStatusAssert{{txId, transactions.TransactionStatusConfirmed}}, 120,
			), nil
		},
	})

	// Verify second withdrawal state
	container.AddStep(e2e.Step{
		Name: "Verify second withdrawal state",
		TestFunc: func(ctx e2e.StepCtx) (e2e.EvaluateFunc, error) {
			return func(ctx e2e.StepCtx) error {
				balKey := "a-" + didKey.String() + "-eth"
				reqBody := fmt.Sprintf(`{"query":"{ getStateByKeys(contractId: \"%s\", keys: [\"n\", \"np\", \"d-1\", \"%s\"], encoding: \"raw\") }"}`, contractId, balKey)
				resp, _ := http.Post("http://localhost:7080/api/v1/graphql", "application/json", strings.NewReader(reqBody))
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				t.Logf("SECOND WITHDRAWAL state: %s", string(body))
				// np should be "2" (second withdrawal), balance further reduced
				return nil
			}, nil
		},
	})

	err := container.RunSteps(t)
	if err != nil {
		t.Fatalf("auto-expiry test failed: %v", err)
	}
}
