package wasm_e2e

import (
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"vsc-node/lib/test_utils"
	contracts "vsc-node/modules/db/vsc/contracts"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	stateEngine "vsc-node/modules/state-processing"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Dir(file) // .../modules/wasm/e2e
	return filepath.Clean(filepath.Join(dir, "..", "..", ".."))
}

func buildAMM(t *testing.T) string {
	t.Helper()
	root := repoRoot(t)
	out := filepath.Join(root, "modules", "e2e", "artifacts", "amm_test.wasm")
	_ = os.MkdirAll(filepath.Dir(out), 0o755)

	// Compile the AMM example using TinyGo
	ammDir := filepath.Clean(filepath.Join(root, "..", "go-contract-template", "examples", "v2-amm"))
	cmd := exec.Command("bash", "-lc", "tinygo build -no-debug -gc=custom -scheduler=none -panic=trap -target=wasm-unknown -o "+out+" .")
	cmd.Dir = ammDir // run from the contract package directory
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("tinygo build failed: %v\n%s", err, string(b))
	}
	return out
}

func TestAMM_Wasm_Init_Add_Swap_Claim(t *testing.T) {
	wasmPath := buildAMM(t)
	code, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read wasm: %v", err)
	}

	ct := test_utils.NewContractTest()
	contractId := "v2amm"
	ct.RegisterContract(contractId, code)

	now := time.Now().UTC().Format(time.RFC3339)

	// helper to craft a tx
	makeTx := func(requiredAuths []string, action string, payload string, intents []map[string]string) stateEngine.TxVscCallContract {
		// wrap intents
		its := make([]contracts.Intent, 0, len(intents))
		for _, m := range intents {
			its = append(its, contracts.Intent{Type: "transfer.allow", Args: m})
		}
		// Self
		self := stateEngine.TxSelf{
			TxId:          hex.EncodeToString([]byte(action + payload + now)),
			BlockId:       "block:e2e",
			BlockHeight:   ct.BlockHeight,
			Index:         0,
			OpIndex:       0,
			Timestamp:     now,
			RequiredAuths: requiredAuths,
		}
		return stateEngine.TxVscCallContract{
			Self:       self,
			NetId:      "vsc-mainnet",
			Caller:     requiredAuths[0],
			ContractId: contractId,
			Action:     action,
			Payload:    []byte(payload),
			RcLimit:    5_000_000_000,
			Intents:    its,
		}
	}

	// init pool hbd/hive, base fee 30 bps
	{
		tx := makeTx([]string{"hive:admin"}, "init", "hbd,hive,30", nil)
		res, _, _ := ct.Call(tx)
		if !res.Success {
			t.Fatalf("init failed: %s", res.Ret)
		}
	}

	// fund alice; deposit ledger balances
	ct.Deposit("hive:alice", 1_000_000, ledgerDb.AssetHbd)
	ct.Deposit("hive:alice", 2_000_000, ledgerDb.AssetHive)

	// add initial liquidity 100k/200k with intents
	{
		intents := []map[string]string{{"limit": "100.000", "token": "hbd"}, {"limit": "200.000", "token": "hive"}}
		tx := makeTx([]string{"hive:alice"}, "add_liquidity", "100000,200000", intents)
		res, _, _ := ct.Call(tx)
		if !res.Success {
			t.Fatalf("add_liquidity failed: %s", res.Ret)
		}
	}

	// swap 0->1: bob swaps 10k hbd to receive hive
	{
		ct.Deposit("hive:bob", 100_000, ledgerDb.AssetHbd)
		intents := []map[string]string{{"limit": "10.000", "token": "hbd"}}
		tx := makeTx([]string{"hive:bob"}, "swap", "0to1,10000", intents)
		res, _, _ := ct.Call(tx)
		if !res.Success {
			t.Fatalf("swap failed: %s", res.Ret)
		}
	}

	// set slip params (system-only) and do another swap to exercise slip fee path
	{
		tx := makeTx([]string{"system:consensus"}, "set_slip_params", "50,500", nil)
		res, _, _ := ct.Call(tx)
		if !res.Success {
			t.Fatalf("set_slip_params failed: %s", res.Ret)
		}
		// non-HBD input should not accrue base fee
		ct.Deposit("hive:carol", 100_000, ledgerDb.AssetHive)
		intents := []map[string]string{{"limit": "100.000", "token": "hive"}}
		tx2 := makeTx([]string{"hive:carol"}, "swap", "1to0,10000", intents)
		res2, _, _ := ct.Call(tx2)
		if !res2.Success {
			t.Fatalf("swap 1to0 failed: %s", res2.Ret)
		}
	}

	// claim fees by reducing contract HBD via withdraw
	{
		pre := ct.GetBalance("contract:v2amm", ledgerDb.AssetHbd)
		tx := makeTx([]string{"system:consensus"}, "claim_fees", "", nil)
		res, _, _ := ct.Call(tx)
		if !res.Success {
			t.Fatalf("claim_fees failed: %s", res.Ret)
		}
		post := ct.GetBalance("contract:v2amm", ledgerDb.AssetHbd)
		if post >= pre {
			t.Fatalf("contract HBD did not decrease on claim: pre=%d post=%d", pre, post)
		}
	}
}

// no-op helper kept for future conversions
var _ = strings.TrimSpace
