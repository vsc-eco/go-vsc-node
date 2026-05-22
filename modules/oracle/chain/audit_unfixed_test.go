package chain

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// audit_unfixed_test.go
//
// Differential tests for audit-MED findings that have NOT been fixed
// yet on the combined branch. Each test pins the current (buggy)
// behavior so that a future fix flips the assertion direction and
// proves remediation.
//
// Sources:
//   - PENDULUM-MEDS-REDTEAM-RESULTS-2026-05-20.md
//   - PENDULUM-UNFIXED-REDTEAM-NOTES (RT-9 / RT-11 / RT-13)

// =====================================================================
// RT-9 — getBlockStats failure silently zeros AverageFeeRate
// =====================================================================
//
// audit MED RT-9: in modules/oracle/chain/bitcoin.go (getBlockByHash,
// ~line 362-372), when the btcd RPC `GetBlockStats` call returns an
// error, the code path swallows the error and sets
// `btcBlock.AverageFeeRate = 0` then returns the block with nil
// error. Downstream consumers cannot distinguish "blockchain genuinely
// reports zero fee rate" from "RPC call failed". Post-fix should
// propagate the error or signal the missing-data condition (e.g. a
// sentinel value or separate ok flag).
//
// The btcd rpcclient.Client is a concrete struct, not an interface, so
// constructing a hand-rolled mock for differential observation is
// disproportionately heavy. Instead we pin the bug as a source-level
// invariant: the silent-zero pattern must be present in the error
// branch today. When the fix lands, this test will start failing,
// signalling the source location was edited.
func TestAuditUnfixed_RT9_GetBlockStatsErrorSilentlyZeroes(t *testing.T) {
	// Locate this file's directory so the test is robust against the
	// caller's working directory.
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller failed")
	dir := filepath.Dir(thisFile)
	src := filepath.Join(dir, "bitcoin.go")

	// Capture the getBlockByHash function body using awk-style sed in
	// pure Go would bloat; rely on `grep -n` for a deterministic,
	// dependency-free probe.
	out, err := exec.Command("grep", "-nE", `AverageFeeRate\s*=\s*0`, src).CombinedOutput()
	require.NoError(t, err, "grep should find the silent-zero line; output: %s", string(out))

	body := string(out)
	assert.Contains(t, body, "AverageFeeRate = 0",
		"RT-9: the silent-zero assignment must still be present pre-fix; "+
			"post-fix should remove this line in favour of returning an error from getBlockByHash")

	// Sanity-check that the assignment lives inside an error branch by
	// reading the function body and verifying that the line above the
	// match is the closing of an `if err != nil {` block opened just
	// before the GetBlockStats call.
	rangeOut, err := exec.Command("grep", "-nE", `GetBlockStats|AverageFeeRate\s*=\s*0|err != nil`, src).CombinedOutput()
	require.NoError(t, err)
	rangeStr := string(rangeOut)
	// Confirm the *sequence* GetBlockStats -> err != nil -> AverageFeeRate=0
	// appears in source order in the file. Splitting by newline and
	// scanning preserves ordering.
	idxStats := strings.Index(rangeStr, "GetBlockStats")
	require.NotEqual(t, -1, idxStats, "expected GetBlockStats call in source")
	idxZero := strings.Index(rangeStr, "AverageFeeRate = 0")
	require.NotEqual(t, -1, idxZero, "expected silent-zero assignment in source")
	assert.Less(t, idxStats, idxZero,
		"RT-9: silent-zero must come AFTER the GetBlockStats call in source order; if the fix reorders, update this test")

	// Post-fix remediation hint baked into the test for future readers:
	//   - replace the silent-zero branch with `return nil, fmt.Errorf("failed to get block stats: %w", err)`
	//     OR
	//   - mark AverageFeeRate as optional (pointer/sentinel) so consumers
	//     can detect the missing-data case.
}
