package election_proposer

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestAuditFix_24_ScoreMapBannedNodesHasLiveCaller — Pendulum audit MEDIUM #24.
//
// Pre-fix: (*electionProposer).scoreMap() ran its under-75%-participation
// computation but was never invoked outside its own def, so BannedNodes was
// dead and persistent under-participators stayed eligible.
//
// Post-fix: GenerateFullElection invokes scoreMap and removes BannedNodes
// from witnessList before the deterministic ordering. This static check
// asserts at least one call site exists. Behavioural verification of the
// filter lives in election-build integration tests.
func TestAuditFix_24_ScoreMapBannedNodesHasLiveCaller(t *testing.T) {
	// Locate the package directory at runtime so this is hermetic across
	// checkouts (no hardcoded /home/dockeruser path).
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	pkgDir := filepath.Dir(thisFile)

	// Sanity: the function still exists. If it gets renamed or removed, this
	// test should fail loudly so the audit finding can be re-graded.
	defFile := filepath.Join(pkgDir, "election-proposer.go")
	defBytes, err := os.ReadFile(defFile)
	if err != nil {
		t.Fatalf("read %s: %v", defFile, err)
	}
	if !strings.Contains(string(defBytes), "func (ep *electionProposer) scoreMap()") {
		t.Fatalf("audit #24: scoreMap definition not found in %s — finding needs re-grading", defFile)
	}

	// Grep all .go files in this package for any reference to scoreMap that
	// isn't (a) the definition line itself or (b) the local variable named
	// `scoreMap` inside the function body. We use word-boundary `\bscoreMap\b`
	// (BRE-equivalent via -w grep flag) and then filter the results in Go.
	cmd := exec.Command("grep", "-rn", "-w", "scoreMap", pkgDir)
	out, _ := cmd.Output() // non-zero exit (no matches) is fine, we'll handle below

	type hit struct {
		file string
		line string
	}
	callers := 0
	var callerHits []hit
	for _, raw := range strings.Split(string(out), "\n") {
		if raw == "" {
			continue
		}
		// grep -rn format: "<path>:<lineno>:<content>"
		parts := strings.SplitN(raw, ":", 3)
		if len(parts) < 3 {
			continue
		}
		file := parts[0]
		content := parts[2]

		// Skip the audit test file itself (it mentions scoreMap in docstrings).
		if strings.HasSuffix(file, "audit_unfixed_test.go") {
			continue
		}

		trimmed := strings.TrimSpace(content)

		// Skip the definition line and its closing-paren signature artifacts.
		if strings.Contains(trimmed, "func (ep *electionProposer) scoreMap()") {
			continue
		}
		// Skip the local variable declaration inside scoreMap's body —
		// `scoreMap := map[string]uint64{}` is not a caller, it's the body.
		if strings.Contains(trimmed, "scoreMap := map[string]uint64") {
			continue
		}
		// Skip in-body reads/writes of the local `scoreMap` map variable.
		// These are uses of the local variable, not of the method.
		if strings.Contains(trimmed, "scoreMap[") {
			continue
		}
		// The return statement assigns `Map: scoreMap,` — also a body use of
		// the local variable.
		if strings.Contains(trimmed, "Map:         scoreMap,") ||
			strings.Contains(trimmed, "Map: scoreMap,") {
			continue
		}

		// Anything left is a real caller (e.g. `ep.scoreMap()`).
		callers++
		callerHits = append(callerHits, hit{file: file, line: trimmed})
	}

	// Post-fix expectation: at least one caller of scoreMap exists
	// (GenerateFullElection wires the BannedNodes filter).
	if callers == 0 {
		t.Fatalf("audit #24: expected at least one caller of scoreMap (BannedNodes filter wired), got 0 — regression")
	}

	t.Logf("audit #24 fix confirmed: %d caller(s) of scoreMap found, sample hits=%v", callers, callerHits)
}
