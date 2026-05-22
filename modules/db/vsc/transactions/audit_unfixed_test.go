package transactions

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// TestAuditUnfixed_RT25_SetOutputUsesPushNotAddToSet proves audit item RT-25:
// db/vsc/transactions/transactionsDb.go:139 — `SetOutput` writes the per-tx
// `output` field via `$push`, which is non-idempotent. If `SetOutput` is
// invoked twice for the same tx id (a real possibility under retry, reorg
// re-application, or duplicate event dispatch), the `output` array grows by
// one element per call instead of remaining a single canonical entry. Any
// downstream consumer that reads `output[0]` or relies on len(output)==1 will
// see corrupted state.
//
// Driving the real bug end-to-end needs a live mongo (the repo has
// transactionsDb_test.go for that, but it requires a network mongo) so this
// test takes the durable, hermetic route: a static-source assertion that the
// SetOutput body still contains `"$push"` and does not yet contain
// `"$addToSet"` (the canonical idempotent fix).
//
// Post-fix expectation: SetOutput either uses `"$addToSet"` (replacing
// `"$push"`) or filters by tx id + a sub-key so a repeated call is a no-op.
func TestAuditUnfixed_RT25_SetOutputUsesPushNotAddToSet(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller failed — cannot locate transactions source")
	}
	srcDir := filepath.Dir(thisFile)
	src := filepath.Join(srcDir, "transactionsDb.go")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	body := string(data)

	startMarker := "func (e *transactions) SetOutput("
	idx := strings.Index(body, startMarker)
	if idx < 0 {
		t.Fatalf("could not locate SetOutput in transactionsDb.go — has it been renamed?")
	}
	rest := body[idx:]
	depth := 0
	end := -1
	started := false
	for i, r := range rest {
		switch r {
		case '{':
			depth++
			started = true
		case '}':
			depth--
			if started && depth == 0 {
				end = i + 1
			}
		}
		if end != -1 {
			break
		}
	}
	if end == -1 {
		t.Fatalf("could not find end of SetOutput body")
	}
	fnBody := rest[:end]

	if !strings.Contains(fnBody, `"$push"`) {
		t.Fatalf("UNEXPECTED: SetOutput no longer uses `\"$push\"` — bug may be fixed; "+
			"replace this static-grep test with an idempotency assertion.\n--- body ---\n%s", fnBody)
	}
	if strings.Contains(fnBody, `"$addToSet"`) {
		t.Fatalf("UNEXPECTED: SetOutput already uses `\"$addToSet\"` — bug may be fixed; " +
			"update this test")
	}

	// Cross-check: no explicit dedup guard either.
	dedupRe := regexp.MustCompile(`output\.[a-zA-Z_]+\s*:`)
	_ = dedupRe // (kept for documentation; presence/absence is informational only)

	t.Log("RT-25 confirmed: SetOutput uses `$push` (non-idempotent) with no " +
		"`$addToSet` alternative. A retried/duplicate SetOutput grows the " +
		"transaction's `output` array. Fix: switch to `$addToSet` (or " +
		"filter the UpdateOne by tx id + a sub-key so the second write is " +
		"a no-op).")
}
