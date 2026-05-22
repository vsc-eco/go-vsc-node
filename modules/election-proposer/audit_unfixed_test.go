package election_proposer

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestAuditUnfixed_24_ScoreMapBannedNodesIsDeadCode — Pendulum audit
// MEDIUM #24.
//
// Precondition: (*electionProposer).scoreMap() computes a list of poorly-
// participating witnesses (under 75% of recent block-signing samples) and
// returns it as ScoreMap.BannedNodes. The full computation runs every time
// scoreMap is invoked — except scoreMap itself is never invoked anywhere
// outside its own definition site. The witness-misbehavior gate it was
// meant to feed (excluding poor performers from the next election) is
// silently dead: any underperformer is still eligible.
//
// This test pins the dead-code precondition via a repo-grep so the next
// refactor that wires up the gate trips the assertion and forces a review.
//
// Post-fix: at least one consumer of scoreMap exists (e.g.
// GenerateFullElection filters election.Members against BannedNodes), and
// the assertion below would flip to require >0 callers.
func TestAuditUnfixed_24_ScoreMapBannedNodesIsDeadCode(t *testing.T) {
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

	// Current (buggy) behavior: zero callers — scoreMap and its BannedNodes
	// output is dead code.
	if callers != 0 {
		t.Fatalf("audit #24: expected 0 callers of scoreMap (current dead-code behavior), got %d:\n%v",
			callers, callerHits)
	}

	// Post-fix the assertion above should flip: at least one caller exists
	// (e.g. GenerateFullElection or makeElection filters its member list
	// through scoreMap().BannedNodes), and the test would assert callers > 0.
	t.Logf("audit #24 confirmed: scoreMap defined at %s but zero in-package callers; ScoreMap.BannedNodes is unreachable.", defFile)
}
