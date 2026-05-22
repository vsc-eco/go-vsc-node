package blockproducer

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// audit_unfixed_test.go
//
// Differential test for audit-MED RT-13 (DOS-by-omission), not yet
// fixed on the combined branch. Pins the current absence of any
// must-include / forced-inclusion rule in BlockProducer's
// generateTransactions, so a future fix that introduces such a rule
// flips the assertion direction.
//
// Source: PENDULUM-UNFIXED-REDTEAM-NOTES (RT-13)

// =====================================================================
// RT-13 — DOS-by-omission, no must-include rule
// =====================================================================
//
// audit MED RT-13: a primary block producer can selectively drop
// (omit) a victim's transaction from a block by simply not appending
// it to `sequencedTxs` in `BlockProducer.generateTransactions`. There
// is no protocol-level "must include" enforcement, no tracking of
// txs that were ingested-but-omitted-N-blocks-running, and no
// downstream slashing or rotation triggered by omission. The audit
// notes that any of those would be acceptable mitigations.
//
// We pin the current state as a source-level invariant: none of
// `mustInclude`, `forcedInclusion`, `omitted` (case-insensitive)
// appears in the generateTransactions source. When a fix lands and
// introduces any of those tokens, this test starts failing and
// directs the reader to flip the assertion direction.
func TestAuditUnfixed_RT13_NoMustIncludeRuleInGenerateTransactions(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller failed")
	dir := filepath.Dir(thisFile)
	src := filepath.Join(dir, "blockProducer.go")

	contents, err := os.ReadFile(src)
	require.NoError(t, err, "read blockProducer.go")
	lower := strings.ToLower(string(contents))

	bannedTokens := []string{
		"mustinclude",
		"forcedinclusion",
		"omitted",
	}

	for _, tok := range bannedTokens {
		assert.NotContains(t, lower, tok,
			"RT-13 (current state): source should not contain %q yet — if it does, the audit-recommended "+
				"must-include / forced-inclusion / omission-tracking has been introduced. Update this test "+
				"to assert the post-fix invariant (e.g. the function returns a non-empty 'forced' set, or "+
				"slashing fires when 'omitted' count exceeds a threshold).", tok)
	}

	// Cross-check: the generateTransactions function itself is still
	// defined here. Guards against the test silently no-op'ing if the
	// function moves to another file.
	assert.Contains(t, string(contents), "func (bp *BlockProducer) generateTransactions(",
		"RT-13: generateTransactions must live in blockProducer.go; if it has moved, update src path")

	// Post-fix remediation hint:
	//   - Add a set of "must-include" txs (e.g. consensus_unstake at
	//     unlock epoch, scheduled withdrawals) that the primary MUST
	//     include or the block is rejected by validators.
	//   - OR track per-producer "omitted N blocks in a row" counters
	//     and slash / rotate primary when threshold is exceeded.
}
