//go:build evm_devnet

// BOT-H FINDING PROOF — proves the contract<->bot scan-height mismatch at
// runtime (test-only; no bot/contract source change). The evm-mapping-bot caps
// its L1 deposit scan at contractHeight = state key "h" read FROM THE
// ACCOUNT-MAPPING CONTRACT (cmd/evm-mapping-bot/main.go:2646), but contract
// refactor M23-F5 (#31) moved "h" to the ZK verifier contract — the
// account-mapping contract no longer stores "h" in its own state
// (blocklist/blocks.go GetLastHeight reads it via the verifier). The bot has no
// verifier id, so contractHeight is ALWAYS 0 -> scanUpTo = min(finalized,0) = 0
// -> the bot maps ZERO deposits on mainnet. Local builds pass (both compile);
// only running them together reveals it.
//
// This test plants a header (which sets "h" in the mock VERIFIER) and asserts:
//   1. account-mapping["h"]  is EMPTY  (the bot's read target — nothing there)
//   2. verifier["h"]         == planted height (where "h" actually lives)
//   3. account-mapping["zkverifier"] == verifier id (the fix path is viable:
//      the bot can read the verifier id from am state, then read "h" from it)
//
// Run: go test -tags evm_devnet -run TestEVMBridge_BotHFinding ./tests/devnet/ -timeout 40m -v
package devnet

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestEVMBridge_BotHFinding_ContractMismatch(t *testing.T) {
	if testing.Short() {
		t.Skip("devnet")
	}
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	t.Cleanup(cancel)

	e := setupEvmBridgeDevnet(t, ctx)

	// Plant a header at a known height — this calls the mock VERIFIER's
	// devSetHeader, which sets KeyLastHeight "h" in the VERIFIER's state.
	const plantedHeight = 1234567
	hdr := &AnvilHeader{
		Number:       plantedHeight,
		StateRoot:    "11111111111111111111111111111111111111111111111111111111111111aa",
		TxRoot:       "22222222222222222222222222222222222222222222222222222222222222bb",
		ReceiptsRoot: "33333333333333333333333333333333333333333333333333333333333333cc",
		BaseFee:      1000000000,
		GasLimit:     30000000,
		Timestamp:    1700000000,
	}
	if err := e.plantHeader(hdr, 1); err != nil {
		t.Fatalf("plantHeader: %v", err)
	}
	t.Logf("planted header height=%d into the mock verifier (%s)", plantedHeight, e.mockID)

	// (1) The account-mapping contract's OWN state has NO "h" — this is exactly
	// what fetchContractLastHeight(cfg.ContractID) reads, so the bot gets 0.
	amState, _ := e.d.GetStateByKeys(ctx, e.gqlNode, e.amID, []string{"h"})
	amH := ""
	if amState != nil && amState["h"] != nil {
		amH = fmt.Sprintf("%v", amState["h"])
	}
	if amH != "" && amH != "0" && amH != "<nil>" {
		t.Fatalf("UNEXPECTED: account-mapping contract DOES have 'h'=%q — the finding may be stale (M23-F5 reverted?). Re-verify.", amH)
	}
	t.Logf("PROVEN (1): account-mapping['h'] = %q (EMPTY) — the bot reads THIS and gets contractHeight=0", amH)

	// (2) The verifier's "h" carries the real height.
	var verifierH string
	if err := pollUntil(ctx, 60*time.Second, func() bool {
		m, _ := e.d.GetStateByKeys(ctx, e.gqlNode, e.mockID, []string{"h"})
		if m == nil || m["h"] == nil {
			return false
		}
		verifierH = fmt.Sprintf("%v", m["h"])
		return verifierH == fmt.Sprintf("%d", plantedHeight)
	}); err != nil {
		t.Fatalf("verifier['h'] never reached planted height %d (got %q)", plantedHeight, verifierH)
	}
	t.Logf("PROVEN (2): verifier['h'] = %q — 'h' lives in the VERIFIER, not account-mapping", verifierH)

	// (3) The account-mapping contract DOES expose the verifier id under
	// "zkverifier" (VerifierContractIdKey) — so the fix is viable WITHOUT new
	// bot config: the bot can read am['zkverifier'], then read 'h' from it.
	zkState, _ := e.d.GetStateByKeys(ctx, e.gqlNode, e.amID, []string{"zkverifier"})
	zkv := ""
	if zkState != nil && zkState["zkverifier"] != nil {
		zkv = fmt.Sprintf("%v", zkState["zkverifier"])
	}
	if zkv != e.mockID {
		t.Fatalf("account-mapping['zkverifier'] = %q, expected verifier id %q — fix path needs re-check", zkv, e.mockID)
	}
	t.Logf("PROVEN (3): account-mapping['zkverifier'] = %q (== verifier id) — fix: bot reads this, then 'h' from the verifier", zkv)

	t.Logf("BOT-H FINDING PROVEN: account-mapping['h'] EMPTY while verifier['h']=%d. "+
		"The bot's fetchContractLastHeight(cfg.ContractID) reads account-mapping['h']=0 -> scanUpTo=min(finalized,0)=0 -> "+
		"maps ZERO deposits. Fix (no new config): bot reads account-mapping['zkverifier']=%s, then reads 'h' from that verifier.",
		plantedHeight, e.mockID)
}
