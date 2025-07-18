package wasm_e2e

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

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
