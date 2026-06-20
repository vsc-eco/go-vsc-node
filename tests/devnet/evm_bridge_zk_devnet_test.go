//go:build evm_devnet

// W-ZK — runtime proof of the REAL zk-review6 header verifier's INPUT-VALIDATION
// reject paths on a 5-node devnet.
//
// Scope: this test deploys the REAL `zk-header-verifier` contract (built from
// med/zk-review6 contract/main.go via the pinned tinygo Makefile, bin/main.wasm)
// and proves that submitProof REJECTS malformed / fake submissions at runtime,
// BEFORE the expensive Sp1VerifyGroth16 host pairing check where it can. It does
// NOT attempt the ACCEPT path — a valid SP1 proof is Milo's GPU prover and is
// explicitly OUT OF SCOPE here (no real proof is constructed or asserted).
//
// Reject paths proven (each a distinct abort in submitProof, contract/main.go):
//
//   (a) #85 shape gate (main.go:361-363) — the cheap pre-verify guard:
//         len(public_values)%2 != 0  ||  len(public_values) < PvMinLenWithChainId*2 (=832)
//       => "public_values too short or odd-length hex".
//       Tested with an odd-length hex ("abc") and a too-short even hex ("00").
//
//   (b) fake/zero proof (main.go:315-369) — a correctly-SHAPED but bogus
//       submission (832 hex zeros = 416 zero bytes) clears the shape gate, then
//       fails the host Sp1VerifyGroth16 ("proof verification failed"). This is
//       the residual-DoS case the shape gate intentionally cannot stop (only the
//       host pairing check distinguishes a valid proof from correctly-sized
//       garbage). The chainId-binding check (main.go:397-399) sits AFTER the
//       groth16 verify, so a fake proof can never reach it — we assert ok==false
//       (either reject is a valid rejection of a fake proof).
//
//   (c) required-fields gate (main.go:315-317):
//         proof=="" || public_values=="" || len(headers)==0
//       => "proof, public_values, and headers required". Tested with empty proof.
//
// The shape gate / required-fields gate run BEFORE any host call, so the proof
// bytes are irrelevant for (a)/(c); for (b) the host is reached and fails on
// garbage. readRLPLen (#65, main.go:635) is the header-side length cap; it is
// only reachable AFTER a successful groth16 verify + chainId match (header
// parsing begins at main.go:423), so it cannot be exercised without Milo's real
// proof and is documented here as out-of-scope-for-runtime (covered by the
// contract's own Go unit tests).
//
// Run: go test -tags evm_devnet -run TestEVMBridge_ZK_VerifierRejectPaths ./tests/devnet/ -timeout 40m -v
package devnet

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

const (
	// REAL zk-review6 header verifier, built from contract/main.go via the
	// pinned tinygo Makefile (docker tinygo/tinygo:0.41.1 @sha256:b216f534...):
	//   cd /home/clauderfly/r6-wt/zk-header-verifier && make build
	// produces bin/main.wasm (the load-bearing artifact for this test).
	zkVerifierWasm = "/home/clauderfly/r6-wt/zk-header-verifier/bin/main.wasm"

	// v6.1.0 SP1 init values (GPU-PROVER-SETUP.md). groth16_vk is NOT required
	// to be the real circuit VK for the reject-path test: every reject case
	// aborts BEFORE (a/c) or AT (b) the host verify, and the host treats the VK
	// as opaque bytes — a non-empty placeholder satisfies the init non-empty
	// guard (main.go:123) and the not-initialized guard (main.go:323-334). The
	// real VK only matters for the ACCEPT path, which is Milo's (out of scope).
	zkSp1VkeyHash = "00f701535d87c17492e85e1b32649f2578a4e02e7e314401c726af21bba2523d"
	zkVkRoot      = "002f850ee998974d6cc00e50cd0814b098c05bfade466d28573240d057f25352"
	// Non-empty placeholder groth16_vk (real binary VK is Milo's; only the
	// accept path consumes it). 64 hex chars of a recognizable sentinel.
	zkGroth16VkPlaceholder = "00f701535d87c17492e85e1b32649f2578a4e02e7e314401c726af21bba2523d"

	// Anchor + chain binding for init. initial_block_hash must be 0x + 64 hex
	// (main.go:184 looksLikeBlockHash), initial_height non-zero (main.go:131),
	// expected_chain_id non-zero (main.go:142). Sepolia = 11155111.
	zkInitialBlockHash = "0x00000000000000000000000000000000000000000000000000000000000000ab"
	zkExpectedChainId  = 11155111
)

// TestEVMBridge_ZK_VerifierRejectPaths deploys the REAL zk-review6 header
// verifier and proves its input-validation reject paths at runtime. The accept
// path (a valid SP1 proof) is Milo's and is OUT OF SCOPE.
func TestEVMBridge_ZK_VerifierRejectPaths(t *testing.T) {
	if testing.Short() {
		t.Skip("devnet")
	}
	t.Parallel()
	requireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()

	// Foundation: running 5-node devnet with the owner (node 1) RC-bootstrapped.
	// We reuse the validated bridge setup (it deploys account-mapping + mock and
	// credits the owner) and then deploy the REAL verifier on top.
	e := setupEvmBridgeDevnet(t, ctx)

	// Deploy the REAL zk-review6 verifier (bin/main.wasm) as a fresh contract.
	// Use deployWithRetry — the contract-deployer's storage-proof request can
	// transiently time out under I/O pressure on a constrained box.
	verifierID, err := deployWithRetry(ctx, e.d, ContractDeployOpts{
		WasmPath:     zkVerifierWasm,
		Name:         "zk-header-verifier",
		DeployerNode: e.ownerNode,
		GQLNode:      e.gqlNode,
	}, 3)
	if err != nil {
		t.Fatalf("deploy zk verifier: %v", err)
	}
	t.Logf("zk verifier deployed: %s", verifierID)

	// --- init the verifier (owner = deployer node) ---
	// All-required fields per InitContractParams (main.go:89-101). The owner is
	// node 1 (the deployer), so the checkOwner() gate (main.go:105) passes.
	initPayload := fmt.Sprintf(
		`{"groth16_vk":"%s","vk_root":"%s","sp1_vkey_hash":"%s","max_retention":10000,"initial_height":1,"initial_block_hash":"%s","is_testnet":true,"expected_chain_id":%d}`,
		zkGroth16VkPlaceholder, zkVkRoot, zkSp1VkeyHash, zkInitialBlockHash, zkExpectedChainId,
	)
	initTx, err := e.callOnce(e.ownerNode, verifierID, "init", initPayload)
	if err != nil {
		t.Fatalf("init zk verifier broadcast: %v", err)
	}
	// Assert init succeeded by observing the KeyGroth16Vk ("vk") state key land.
	if err := pollUntil(ctx, 3*time.Minute, func() bool {
		m, _ := e.d.GetStateByKeys(ctx, e.gqlNode, verifierID, []string{"vk"})
		return m != nil && m["vk"] != nil && fmt.Sprintf("%v", m["vk"]) != ""
	}); err != nil {
		dumpMapErr(t, e, ctx, initTx)
		t.Fatalf("zk verifier init did not install vk state (init tx %s)", initTx)
	}
	// Also confirm the bound chainId ("cid") and anchor ("lbh") were set, so we
	// know the contract is in the fully-initialized state that submitProof's
	// guards expect (otherwise a reject could be a not-initialized false pass).
	if m, _ := e.d.GetStateByKeys(ctx, e.gqlNode, verifierID, []string{"cid", "lbh"}); m != nil {
		t.Logf("zk verifier initialized: vk set, cid=%v lbh=%v", m["cid"], m["lbh"])
	}

	// zkSubmit broadcasts a single submitProof call (non-idempotent path; no
	// retry) and returns its tx id.
	zkSubmit := func(proof, publicValues, headersJSON string) (string, error) {
		payload := fmt.Sprintf(`{"proof":"%s","public_values":"%s","headers":%s}`, proof, publicValues, headersJSON)
		return e.callOnce(e.ownerNode, verifierID, "submitProof", payload)
	}

	// assertReject submits a malformed/fake proof and asserts the contract
	// REJECTED it (ok==false). If wantSubstrs is non-empty, at least one must
	// appear in the reject message (the abort string surfaced via the contract
	// output DAG). The contract-output read reuses the validated outResult
	// helper (findContractOutput.ret, falling back to the raw output DAG ->
	// results[].{ok,errMsg,ret}) — the same walk as zkOutMsg in the task brief.
	assertReject := func(label, proof, pv, headersJSON string, wantSubstrs ...string) {
		t.Helper()
		tx, err := zkSubmit(proof, pv, headersJSON)
		if err != nil {
			t.Fatalf("[%s] submitProof broadcast: %v", label, err)
		}
		ok, msg := e.outResult(ctx, tx)
		if ok {
			t.Fatalf("[%s] submitProof was ACCEPTED (ok=true) — expected reject; msg=%q tx=%s", label, msg, tx)
		}
		if len(wantSubstrs) > 0 {
			matched := false
			for _, s := range wantSubstrs {
				if strings.Contains(msg, s) {
					matched = true
					break
				}
			}
			if !matched {
				t.Fatalf("[%s] rejected (ok=false) but message %q did not contain any of %v (tx=%s)", label, msg, wantSubstrs, tx)
			}
		}
		t.Logf("[%s] REJECTED ok=false msg=%q", label, msg)
	}

	const headersOne = `[{"rlp_hex":"00"}]`

	// (a) #85 shape gate — odd-length hex public_values.
	//     proof + headers non-empty so the required-fields gate (b/c order) is
	//     passed; "abc" (len 3, odd) trips the shape gate at main.go:361.
	assertReject("shape-gate/odd-length", "00", "abc", headersOne,
		"public_values too short or odd-length hex")

	// (a) #85 shape gate — too-short (even) hex public_values.
	//     "00" (len 2, even, < 832) trips the same gate.
	assertReject("shape-gate/too-short", "00", "00", headersOne,
		"public_values too short or odd-length hex")

	// (b) fake/zero proof — 832 hex zeros (416 zero bytes) clears the shape gate
	//     (832 == PvMinLenWithChainId*2, even), then fails the host
	//     Sp1VerifyGroth16 with garbage => "proof verification failed". The
	//     chainId-binding check (main.go:397) is AFTER the host verify, so a fake
	//     proof can never reach it; either reject (verify-fail OR chainId) is a
	//     valid rejection of a fake proof. We accept either substring.
	zeroPv := strings.Repeat("0", PvMinLenWithChainIdHexLen) // 832 '0' chars
	assertReject("fake-proof/zero-pv", "00", zeroPv, headersOne,
		"proof verification failed", "does not match verifier's bound chainId")

	// (c) required-fields gate — empty proof (main.go:315-317).
	assertReject("required-fields/empty-proof", "", zeroPv, headersOne,
		"proof, public_values, and headers required")

	// (c) required-fields gate — empty headers array.
	assertReject("required-fields/empty-headers", "00", zeroPv, `[]`,
		"proof, public_values, and headers required")

	t.Log("ZK PASS — verifier reject paths: " +
		"shape-gate(odd-length + too-short) rejected with 'public_values too short or odd-length hex'; " +
		"fake/zero-pv proof rejected at host groth16 verify (or chainId bind); " +
		"required-fields(empty-proof + empty-headers) rejected with 'proof, public_values, and headers required'. " +
		"accept path = Milo's real SP1 proof (OUT OF SCOPE — not attempted).")
}

// PvMinLenWithChainIdHexLen is the hex-character length of a public_values blob
// that exactly clears the #85 shape gate: PvMinLenWithChainId (416 bytes) * 2.
// Kept local to this test (the contract const is in the verifier package, not
// importable here).
const PvMinLenWithChainIdHexLen = 416 * 2 // 832
