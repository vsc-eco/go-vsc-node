package devnet

// TestGeneratePreParamsFixture writes pre-generated TSS pre-parameter sets for
// Config.PreParamsFixtureDir. One-time and resumable: it tops the directory up to
// GEN_PREPARAMS sets. Every set is round-tripped through JSON and validated before
// it is written, and written atomically.
//
//	GEN_PREPARAMS=120 DEVNET_PREPARAMS_DIR=$HOME/.cache/magi-devnet-preparams \
//	  go test -run TestGeneratePreParamsFixture -v -timeout 120m ./tests/devnet/

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"vsc-node/modules/tss"

	ecKeyGen "github.com/bnb-chain/tss-lib/v3/ecdsa/keygen"
)

func TestGeneratePreParamsFixture(t *testing.T) {
	want, _ := strconv.Atoi(os.Getenv("GEN_PREPARAMS"))
	if want <= 0 {
		t.Skip("set GEN_PREPARAMS=<count> and DEVNET_PREPARAMS_DIR")
	}
	dir := os.Getenv("DEVNET_PREPARAMS_DIR")
	if dir == "" {
		t.Fatal("DEVNET_PREPARAMS_DIR is not set")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	have := 0
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "set-") && strings.HasSuffix(e.Name(), ".json") {
			have++
		}
	}
	if have >= want {
		t.Logf("%s already holds %d sets", dir, have)
		return
	}

	// Each generation searches for safe primes on two goroutines.
	workers := runtime.NumCPU() / 2
	if workers < 1 {
		workers = 1
	}
	jobs := make(chan int)
	var done atomic.Int64
	var wg sync.WaitGroup
	start := time.Now() // wall: measures fixture generation
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				p, err := ecKeyGen.GeneratePreParams(20*time.Minute, 2)
				if err != nil {
					t.Errorf("set %d: %v", i, err)
					continue
				}
				raw, err := json.Marshal(p)
				if err != nil {
					t.Errorf("set %d: marshal: %v", i, err)
					continue
				}
				// Validate the set exactly as a node will after loading it.
				var back ecKeyGen.LocalPreParams
				if err := json.Unmarshal(raw, &back); err != nil {
					t.Errorf("set %d: JSON round-trip: %v", i, err)
					continue
				}
				if err := tss.ValidateDevnetPreParams(&back); err != nil {
					t.Errorf("set %d: invalid after JSON round-trip: %v", i, err)
					continue
				}
				if back.PaillierSK.N.Cmp(p.PaillierSK.N) != 0 || back.NTildei.Cmp(p.NTildei) != 0 {
					t.Errorf("set %d: JSON round-trip changed the moduli", i)
					continue
				}
				name := filepath.Join(dir, fmt.Sprintf("set-%04d.json", i))
				if err := os.WriteFile(name+".tmp", raw, 0o644); err != nil {
					t.Errorf("set %d: %v", i, err)
					continue
				}
				if err := os.Rename(name+".tmp", name); err != nil {
					t.Errorf("set %d: %v", i, err)
					continue
				}
				n := done.Add(1)
				t.Logf("set %d written (%d/%d new, %s elapsed)", i, n, want-have, time.Since(start).Round(time.Second)) // wall: measures fixture generation
			}
		}()
	}
	for i := have; i < want; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	t.Logf("generated %d sets in %s with %d workers", done.Load(), time.Since(start).Round(time.Second), workers) // wall: measures fixture generation
}
