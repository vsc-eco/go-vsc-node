package devnet

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeDeployer puts a docker on PATH whose `compose ... run` prints the
// deployer's no-storage-proof output for the first fails runs and a deployed
// contract after that. It returns the file that counts the runs.
func fakeDeployer(t *testing.T, fails int) (*Devnet, string) {
	t.Helper()
	dir := t.TempDir()
	runs := filepath.Join(dir, "runs")
	script := fmt.Sprintf(`#!/bin/sh
case " $* " in *" run "*)
  echo x >> %[1]s
  n=$(/usr/bin/wc -l < %[1]s)
  if [ "$n" -le %[2]d ]; then
    echo "code: 8575 [0 97 115 109] ..."
    echo "failed to request storage proof context deadline exceeded"
  else
    echo "storage proof {bafk...}"
    echo "contract id: vsc1Deployed$n"
  fi;;
esac
exit 0
`, runs, fails)
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	wasm := filepath.Join(dir, "c.wasm")
	os.WriteFile(wasm, []byte("\x00asm"), 0o644)
	cfg := DefaultConfig()
	cfg.DataDir = filepath.Join(dir, "devnet")
	d, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	d.started = true
	return d, runs
}

func deployRuns(t *testing.T, runs string) int {
	b, _ := os.ReadFile(runs)
	return strings.Count(string(b), "x")
}

// A deploy whose deployer got no storage proof broadcast nothing, so it is run
// again (FD-27), and the contract it then deploys is returned.
func TestDeployContractRetriesAMissingStorageProof(t *testing.T) {
	d, runs := fakeDeployer(t, 1)
	cid, err := d.DeployContract(context.Background(), ContractDeployOpts{WasmPath: filepath.Join(filepath.Dir(runs), "c.wasm"), Name: "c"})
	if err != nil {
		t.Fatalf("deploy after one missing storage proof: %v", err)
	}
	if cid != "vsc1Deployed2" || deployRuns(t, runs) != 2 {
		t.Fatalf("got %q after %d deployer runs, want vsc1Deployed2 after 2", cid, deployRuns(t, runs))
	}
}

func TestDeployContractGivesUpAfterThreeMissingProofs(t *testing.T) {
	d, runs := fakeDeployer(t, 99)
	_, err := d.DeployContract(context.Background(), ContractDeployOpts{WasmPath: filepath.Join(filepath.Dir(runs), "c.wasm"), Name: "c"})
	if err == nil || !strings.Contains(err.Error(), "failed to request storage proof") {
		t.Fatalf("err = %v, want the deployer's storage-proof failure", err)
	}
	if n := deployRuns(t, runs); n != deployAttempts {
		t.Fatalf("deployer ran %d times, want %d", n, deployAttempts)
	}
}

func TestStorageProofFailed(t *testing.T) {
	cases := map[string]bool{
		"code: 1 [0]\nfailed to request storage proof context deadline exceeded\n": true,
		"storage proof {x}\ncontract id: vsc1Abc\n":                                false,
		"failed to read WASM file open /wasm/x: no such file\n":                    false,
	}
	for out, want := range cases {
		if got := storageProofFailed(out); got != want {
			t.Errorf("%q: got %v, want %v", out, got, want)
		}
	}
}
