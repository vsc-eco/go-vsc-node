package devnet

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// wallClockCall matches the calls that read or wait on wall clock.
var wallClockCall = regexp.MustCompile(`\btime\.(Now|Sleep|After|Since|Until|Tick|NewTimer|NewTicker|AfterFunc)\(|\bcontext\.(WithTimeout|WithDeadline)\(`)

// wallClockEngine are the files that drive or test block cadence itself, where
// wall clock is the subject.
var wallClockEngine = map[string]bool{
	"pacemaker.go":           true,
	"pacemaker_test.go":      true,
	"testclock_lint_test.go": true,
}

// Every wait, deadline and phase budget in this package runs on the test clock
// (devclock.go), so a FastBlocks run gives each the same number of blocks a live
// chain would. A line that needs wall clock says why in a "// wall:" comment.
func TestDevnetCodeUsesTheTestClock(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no Go files found (%v)", err)
	}
	checked := 0
	for _, name := range files {
		if wallClockEngine[name] {
			continue
		}
		f, err := os.Open(name)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for ln := 1; sc.Scan(); ln++ {
			line := sc.Text()
			code, _, _ := strings.Cut(line, "//")
			if strings.TrimSpace(code) == "" || !wallClockCall.MatchString(code) {
				continue
			}
			checked++
			if !strings.Contains(line, "// wall:") {
				t.Errorf("%s:%d waits on wall clock; use devnetNow/devnetSince/devnetSleep/devnetAfter/devnetWithTimeout, or say why in a \"// wall:\" comment:\n\t%s",
					name, ln, strings.TrimSpace(line))
			}
		}
		f.Close()
		if err := sc.Err(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if checked == 0 {
		t.Fatal("found no wall-clock calls at all, so the scan is not looking at this package")
	}
}
