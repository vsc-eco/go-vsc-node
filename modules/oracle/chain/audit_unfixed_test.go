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
// Post-fix companion for audit-MED RT-9.
//
// Pre-fix: getBlockByHash swallowed a GetBlockStats RPC error and set
// AverageFeeRate=0, then returned nil error. A btcd flake surfaced as a
// "0 sat/vB" fee on the BTC unmap path and signed a stuck mainnet tx.
//
// Post-fix: the error is propagated. The silent-zero assignment is gone.

// TestAuditFix_RT9_GetBlockStatsErrorIsPropagated reads the source file
// and confirms the silent-zero assignment is gone AND the error path now
// returns via fmt.Errorf.
func TestAuditFix_RT9_GetBlockStatsErrorIsPropagated(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller failed")
	dir := filepath.Dir(thisFile)
	src := filepath.Join(dir, "bitcoin.go")

	// 1. The silent-zero line must be gone.
	out, _ := exec.Command("grep", "-nE", `AverageFeeRate\s*=\s*0`, src).CombinedOutput()
	body := strings.TrimSpace(string(out))
	if body != "" {
		t.Fatalf("RT-9: silent-zero assignment still present in %s — fix has regressed:\n%s", src, body)
	}

	// 2. The GetBlockStats error branch must now return an error.
	rangeOut, err := exec.Command("grep", "-nE", `GetBlockStats|return nil, fmt\.Errorf.*block stats|err != nil`, src).CombinedOutput()
	require.NoError(t, err)
	rangeStr := string(rangeOut)
	idxStats := strings.Index(rangeStr, "GetBlockStats")
	require.NotEqual(t, -1, idxStats, "expected GetBlockStats call still present")
	idxReturn := strings.Index(rangeStr, "block stats")
	assert.NotEqual(t, -1, idxReturn,
		"RT-9: expected post-fix to return a wrapped error mentioning \"block stats\" — got grep output:\n%s", rangeStr)
	assert.Less(t, idxStats, idxReturn,
		"RT-9: error-return must come AFTER the GetBlockStats call")
}
