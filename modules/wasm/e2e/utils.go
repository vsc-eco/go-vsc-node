package wasm_e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

var (
	compiledOnce sync.Once
	compiledCode []byte
	compiledErr  error
)

// compiledContract builds the test contract (go_wasm) once per test process
// and returns its bytecode. For tests that need the contract as it is in this
// tree; reading tmp/main.wasm directly gives whatever was built last.
//
// It builds into a directory of its own, not tmp/main.wasm: packages are tested
// in parallel, and a second process writing that file while this package's
// tests read it would hand them half a contract.
func compiledContract(t *testing.T) []byte {
	t.Helper()
	root := projectRoot(t)
	compiledOnce.Do(func() {
		var dir string
		dir, compiledErr = os.MkdirTemp("", "vsc-wasm-e2e-")
		if compiledErr != nil {
			return
		}
		defer os.RemoveAll(dir)
		path := filepath.Join(dir, "main.wasm")
		// The compiler's output goes into the error: every later test in the
		// process reports this one cached result.
		if out, err := buildCmd(root, path).CombinedOutput(); err != nil {
			compiledErr = fmt.Errorf("%w (tinygo must be on PATH): %s", err, strings.TrimSpace(string(out)))
			return
		}
		compiledCode, compiledErr = os.ReadFile(path)
	})
	if compiledErr != nil {
		t.Fatalf("compile the test contract: %v", compiledErr)
	}
	return compiledCode
}

func hasGoMod(dir string) bool {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.Name() == "go.mod" {
			return true
		}
	}
	return false
}

func projectRoot(t *testing.T) string {
	_, file, _, ok := runtime.Caller(0)
	wd := file
	if !ok {
		t.Fatal("could not find root caller source file")
	}
	for !hasGoMod(wd) {
		prevWd := wd
		wd = filepath.Dir(wd)
		if wd == prevWd {
			t.Fatal("could not find root of project in this test. This is a bug with the test.")
		}
	}
	return wd
}
