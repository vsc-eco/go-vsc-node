package pendulum

// Audit finding #59 (unfixed): modules/incentive-pendulum/slashing.go
// exports OracleEvidence, SlashParams, SlashBpsRaw, SlashBps,
// BlockProductionDeficit — but NO production code (outside
// slashing_test.go) calls any of them. The slashing module is fully
// dead code: oracle evidence is never aggregated, no actor invokes
// SlashBps with real evidence, and no balance/stake is ever debited.
//
// Reachability: equivocating or absent oracles incur zero economic
// penalty in production, despite the algorithm being present and
// unit-tested. Any deployment that assumes "slashing is enabled
// because the module exists" is mistaken.
//
// This test demonstrates the *precondition* by walking modules/ and
// asserting that the only non-test callers of these symbols live in
// slashing.go itself. When the fix lands (a real caller in
// state-processing / oracle / election aggregation), this test will
// fail — update it to assert the new caller is wired correctly.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAuditUnfixed_59_SlashingHasNoProductionCallers(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot determine caller file")
	}
	// thisFile = .../modules/incentive-pendulum/audit_unfixed_59_test.go
	modulesDir := filepath.Join(filepath.Dir(thisFile), "..")
	abs, err := filepath.Abs(modulesDir)
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	modulesDir = abs

	symbols := []string{
		"OracleEvidence",
		"SlashBpsRaw",
		"SlashBps",
		"BlockProductionDeficit",
	}

	type hit struct {
		Path   string
		Symbol string
	}
	var callers []hit

	err = filepath.Walk(modulesDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// Skip _test.go files (tests of the dead code don't count as
		// production callers).
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// Skip slashing.go itself (the definitions).
		if strings.HasSuffix(path, "incentive-pendulum/slashing.go") ||
			strings.HasSuffix(path, "incentive-pendulum\\slashing.go") {
			return nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		src := string(raw)
		for _, sym := range symbols {
			if strings.Contains(src, sym) {
				callers = append(callers, hit{Path: path, Symbol: sym})
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if len(callers) != 0 {
		// Fix has likely landed — a production path now uses slashing.
		t.Fatalf("expected ZERO non-test callers of slashing symbols outside slashing.go, but found: %+v", callers)
	}
	t.Logf("precondition confirmed: slashing module has no production callers (dead code)")
}
