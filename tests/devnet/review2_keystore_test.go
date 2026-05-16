package devnet

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestReview2KeystoreEncryptedAtRest is the targeted end-to-end proof for
// review2 CRITICAL #4 (cleartext TSS key shares on disk).
//
// It is a differential test:
//   - On the #170 baseline (pre-fix) the DKG share is written to the flatfs
//     keystore as plaintext json.Marshal(savedOutput) — this test FAILS (RED),
//     demonstrating the vulnerability on a real 5-node network.
//   - On fix/review2 the share is the AES-256-GCM envelope (magic "VSCK1\0")
//     written by keystorePutEncrypted — this test PASSES (GREEN).
//
// It spins a real devnet, waits for keygen to complete (which means each node
// has run DKG and persisted its secret share to
// <dataDir>/devnet-data/data-<N>/tss-keys/**/*.data), then inspects those
// on-disk share files directly from the host.
func TestReview2KeystoreEncryptedAtRest(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}

	cfg := tssTestConfig()
	d, _ := startDevnet(t, cfg, 15*time.Minute)

	t.Log("waiting for keygen (DKG share persisted to flatfs keystore)...")
	kg := waitForCommitment(t, d.MongoURI(), "keygen", 15*time.Minute)
	t.Logf("keygen: keyId=%s epoch=%d block=%d", kg.KeyId, kg.Epoch, kg.BlockHeight)

	keystoreRoot := filepath.Join(d.DataDir(), "devnet-data")

	// The share may take a moment to flush after the commitment lands.
	var shareFiles []string
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		shareFiles = findTSSShareFiles(t, keystoreRoot)
		if len(shareFiles) > 0 {
			break
		}
		time.Sleep(3 * time.Second)
	}
	if len(shareFiles) == 0 {
		t.Fatalf("no TSS keystore share files (*.data under data-*/tss-keys) found below %s — keygen did not persist a share", keystoreRoot)
	}
	t.Logf("found %d on-disk TSS share file(s)", len(shareFiles))

	// Plaintext markers that appear in a json.Marshal'd ECDSA
	// keyGenSecp256k1.LocalPartySaveData (the pre-fix on-disk format).
	plaintextMarkers := []string{`"PaillierSK"`, `"Xi"`, `"NTildei"`, `"ECDSAPub"`}

	for _, f := range shareFiles {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("reading share file %s: %v", f, err)
		}
		if len(b) == 0 {
			continue // not yet flushed; ignore empty
		}

		encrypted := bytes.HasPrefix(b, []byte("VSCK1\x00"))

		// Strong, explicit failure message for the pre-fix (RED) case.
		if !encrypted {
			leak := ""
			for _, m := range plaintextMarkers {
				if bytes.Contains(b, []byte(m)) {
					leak = m
					break
				}
			}
			head := string(b)
			if len(head) > 80 {
				head = head[:80]
			}
			t.Fatalf("review2 C4: TSS key share %s is NOT encrypted at rest "+
				"(no VSCK1 envelope; plaintext marker=%q; head=%q). The private "+
				"share is readable on disk.", f, leak, strings.TrimSpace(head))
		}

		// Belt-and-suspenders: an encrypted blob must not contain the
		// plaintext share field names.
		for _, m := range plaintextMarkers {
			if bytes.Contains(b, []byte(m)) {
				t.Fatalf("share %s has the VSCK1 prefix but still contains plaintext marker %q", f, m)
			}
		}
	}

	t.Logf("OK: all %d TSS share file(s) are AES-256-GCM encrypted at rest (VSCK1 envelope)", len(shareFiles))
}

// findTSSShareFiles walks every data-*/tss-keys tree under root and returns
// all flatfs value files (".data").
func findTSSShareFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(root, func(path string, de fs.DirEntry, err error) error {
		if err != nil {
			return nil // tolerate transient dir races
		}
		if de.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".data") {
			return nil
		}
		// must be inside a ".../tss-keys/..." subtree
		if !strings.Contains(filepath.ToSlash(path), "/tss-keys/") {
			return nil
		}
		out = append(out, path)
		return nil
	})
	return out
}
