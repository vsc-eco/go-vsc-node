package devnet

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"

	"github.com/vsc-eco/hivego"
)

// SC-08 devnet proof — a malformed contract update must not halt the network.
//
// THE BUG. `runtime` and `storage_proof` are `omitempty` pointers on
// TxUpdateContract, so a vsc.update_contract custom_json that omits either --
// or sends an explicit null -- unmarshals to a nil pointer and still reaches
// the code-update branch, where both are dereferenced
// (tx.Runtime.String() auto-derefs because Runtime.String has a value
// receiver; StorageProof.Verify reads sp.Hash).
//
// That panic is not survivable. It unwinds out of ProcessBlock, and the L1
// system-op path -- unlike the L2 batch path, which routes every op through
// executeTxSafely -- had no recover() anywhere on it, so the process dies. And
// the streamer only advances its saved checkpoint after a block completes, so
// on restart the node re-ingests the same block and dies again. Every node that
// reaches that block stops, permanently, for the price of the deployment fee.
//
// WHY A DEVNET. The unit tests prove the handler now rejects the payload
// instead of panicking, and that the recover() contains a panic. Neither can
// show the thing that actually mattered: that the NETWORK keeps producing
// blocks after ingesting the op. That needs real nodes ingesting a real L1
// transaction.
//
// SHAPE OF THE ATTACK. Two ops in one L1 transaction, because
// hasFeePaymentOp requires the fee transfer to be ops[1]:
//
//	ops[0] custom_json vsc.update_contract  — no "runtime" key
//	ops[1] transfer 10 TBD -> vsc.gateway   — the deployment fee, so hasFee is
//	                                          true and the code-update branch
//	                                          (the one that derefs) is entered
//
// The update also has to name a contract the signer owns, so the test deploys
// one first and then points the malformed update at it.
func TestSC08MalformedUpdateDevnet(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}
	requireDocker(t)

	cfg := DefaultConfig()
	cfg.Nodes = 4
	cfg.GenesisNode = 4
	cfg.LogLevel = "info"
	cfg.SysConfigOverrides = &systemconfig.SysConfigOverrides{
		ConsensusParams: &params.ConsensusParams{ElectionInterval: 20},
	}

	d, ctx := startDevnetNoKey(t, cfg, 30*time.Minute)
	t.Logf("SC-08: devnet running: GQL=%s", d.GQLEndpoint(1))

	heights := func() []uint64 {
		out := make([]uint64, cfg.Nodes)
		for n := 1; n <= cfg.Nodes; n++ {
			if h, _, err := d.LocalNodeInfo(ctx, n); err == nil {
				out[n-1] = h
			}
		}
		return out
	}

	// ── Phase 1: a contract the signer owns ────────────────────────────────
	//
	// The update handler returns "failed to retrieve contract to update" for an
	// unknown id and "not owner" for someone else's, both BEFORE the deref. So
	// without a real owned contract the test would pass without ever reaching
	// the vulnerable branch.
	wasmPath, err := BuildCallTssContract(ctx)
	if err != nil {
		t.Fatalf("SC-08 BLOCKED: building the target contract wasm: %v", err)
	}

	contractID, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath:     wasmPath,
		Name:         "sc08-target",
		Description:  "SC-08 malformed-update target",
		DeployerNode: 1,
		GQLNode:      2,
	})
	if err != nil {
		t.Fatalf("SC-08 BLOCKED: deploying the target contract: %v", err)
	}
	t.Logf("SC-08: target contract deployed: %s", contractID)

	// Fund the owner so the 10 TBD fee transfer can go out.
	owner := d.witnessAccount(1)

	base := heights()
	t.Logf("SC-08: baseline heights %v", base)

	// ── Phase 2: broadcast the malformed update + its fee, in one L1 tx ────
	//
	// NOTE the absent "runtime": that is the whole payload. "code" must differ
	// from the existing code or the branch is skipped (tx.Code != existing.Code).
	updateJSON := `{"net_id":"` + d.netId() + `","id":"` + contractID +
		`","name":"sc08","description":"malformed","code":"bafyreiasc08malformedupdatetestcidvalue000000000000000000000"}`

	ops := []hivego.HiveOperation{
		hivego.CustomJsonOperation{
			RequiredAuths:        []string{owner},
			RequiredPostingAuths: []string{},
			Id:                   "vsc.update_contract",
			Json:                 updateJSON,
		},
		hivego.TransferOperation{
			From:   owner,
			To:     params.GATEWAY_WALLET,
			Amount: "10.000 TBD",
			Memo:   "sc08 deployment fee",
		},
	}

	hiveClient := hivego.NewHiveRpc([]string{d.DroneEndpoint()})
	hiveClient.ChainID = devnetChainID
	wif := d.cfg.InitminerWIF
	txId, err := broadcastRetryingStale(hiveClient, ops, &wif, "sc08 malformed update")
	if err != nil {
		t.Fatalf("SC-08 BLOCKED: broadcasting the malformed update: %v", err)
	}
	t.Logf("SC-08: malformed vsc.update_contract broadcast (no \"runtime\"), L1 tx=%s", txId)

	// ── Phase 3: every node must keep advancing ───────────────────────────
	//
	// THE ASSERTION. Pre-fix every node that ingests this L1 block panics out of
	// ProcessBlock and dies; the heights freeze and never recover, because the
	// checkpoint was never advanced past the block.
	const settle = 2 * time.Minute
	pre := heights()
	t.Logf("SC-08: heights at broadcast time: %v", pre)
	devnetSleep(settle)
	post := heights()
	t.Logf("SC-08: heights after %s: %v", settle, post)

	stalled := make([]int, 0, cfg.Nodes)
	for n := 1; n <= cfg.Nodes; n++ {
		if post[n-1] <= pre[n-1] {
			stalled = append(stalled, n)
		}
	}
	if len(stalled) > 0 {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("SC-08 FAIL: nodes %v stopped advancing after a malformed "+
			"vsc.update_contract (before %v, after %v). That is the permissionless halt: "+
			"a nil Runtime/StorageProof deref panicking out of an unrecovered ProcessBlock.",
			stalled, pre, post)
	}

	// Nodes must also still be HEALTHY, not merely ticking: a node that died and
	// was restarted by docker would re-ingest the same block and die again, so a
	// brief advance is not enough on its own.
	assertAllNodesHealthy(t, d, ctx, healthDelta)

	// ── Phase 4: prove the op actually REACHED the guard ──────────────────
	//
	// Without this the test is worthless. "All nodes still advancing" is also
	// what you see if the malformed op was filtered long before the dereference
	// -- a missing fee op (hasFee false), an unknown contract id, a not-owner
	// check -- in which case the vulnerable branch was never entered and the
	// test proves nothing about the fix.
	//
	// The handler's rejection reason is the evidence. It is only produced inside
	// the code-update branch, i.e. after hasFee, the contract lookup and the
	// owner check have all passed.
	const want = "runtime is required to update contract code"
	reached := false
	var sampled string
	for n := 1; n <= cfg.Nodes; n++ {
		logs, lerr := d.Logs(ctx, fmt.Sprintf("magi-%d", n))
		if lerr != nil {
			continue
		}
		if strings.Contains(logs, want) {
			reached = true
			break
		}
		if strings.Contains(logs, "vsc.update_contract rejected") {
			sampled = "rejected, but for another reason"
		}
	}
	if !reached {
		t.Fatalf("SC-08 INCONCLUSIVE: no node logged %q (%s). The nodes stayed up, but "+
			"nothing shows the malformed op reached the code-update branch, so this run "+
			"does not demonstrate the fix. Check hasFee (the transfer must be ops[1]), "+
			"the contract id, and that the signer owns it.", want, sampled)
	}

	t.Logf("SC-08 PASS: the malformed op reached the guard and was rejected with %q, "+
		"and all %d nodes advanced and stayed healthy", want, cfg.Nodes)
}
