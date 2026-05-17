package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// review2 #122 — the signing private key was the 7th positional CLI arg
// to zk-tx-signer, world-readable via /proc/<pid>/cmdline and `ps`. The
// fix accepts it from ZK_TX_SIGNER_PRIVKEY instead (argv kept only as a
// deprecated rollout-compat fallback, with a warning).
//
// Differential, observed by execing the built binary (no new symbol
// referenced, so this test compiles on both arms): invoked with the 6
// non-key positional args and the key supplied via env, the #170
// baseline still demands 8 args and prints the "Usage:" arg-count
// rejection (RED — the env key is ignored); the fix accepts the env key
// and proceeds past the arg gate, failing later on key/output parsing
// (GREEN — never the "Usage:" rejection, and the key is never required
// on argv).
func TestReview2PrivKeyFromEnvNotArgv(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "zk-tx-signer")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build zk-tx-signer: %v\n%s", err, out)
	}

	// 6 positional args (no key on the command line) + key via env.
	cmd := exec.Command(bin, "contract:test", "submitProof", "{}", "vsc-mainnet", "100", "0")
	cmd.Env = append(os.Environ(), "ZK_TX_SIGNER_PRIVKEY=deadbeef")
	out, _ := cmd.CombinedOutput() // expected to fail later; we assert on *why*
	s := string(out)

	if strings.Contains(s, "Usage:") {
		t.Fatalf("review2 #122: binary rejected env-supplied key with the arg-count "+
			"\"Usage:\" gate — the baseline ignores ZK_TX_SIGNER_PRIVKEY and still "+
			"requires the key as argv[7]. Output:\n%s", s)
	}

	// The key must NOT be required as a positional arg anymore: there are
	// only 6 positional args here, so any failure must be downstream
	// (bad key / signing / network), never the missing-arg usage gate.
	if strings.Contains(s, "<privateKeyHex>") && strings.Contains(s, "Usage:") {
		t.Fatalf("review2 #122: still demands a positional private key. Output:\n%s", s)
	}
}
