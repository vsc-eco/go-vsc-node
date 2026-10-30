package devnet

import (
	"errors"
	"fmt"
	"os"
	"testing"
)

// TestMain fails the package run if any devnet recorded a fast-mode integrity
// failure (pacemaker.go, preparams_pool.go), including runs whose test discards
// the error from Stop, as many cleanups do.
func TestMain(m *testing.M) {
	code := m.Run()
	if failures := IntegrityFailures(); len(failures) > 0 {
		fmt.Fprintf(os.Stderr, "\nFAST-MODE INTEGRITY FAILURES (%d): these runs cannot be trusted\n", len(failures))
		for _, f := range failures {
			fmt.Fprintf(os.Stderr, "  - %s\n", f)
		}
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

// stopDevnetChecked tears d down and fails t on a fast-mode integrity failure;
// any other teardown error stays a warning, as before.
func stopDevnetChecked(t *testing.T, d *Devnet) {
	err := d.Stop()
	if err == nil {
		return
	}
	var ie *IntegrityError
	if errors.As(err, &ie) {
		t.Errorf("%v", err)
		return
	}
	t.Logf("warning: stop failed: %v", err)
}
