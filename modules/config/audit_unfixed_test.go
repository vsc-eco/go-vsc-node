package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"vsc-node/modules/config"
)

// audit CRYP-13 — stripWorld migration only runs on Update, not on Init.
//
// modules/config/config.go:80-170. Config.Init() opens an existing config
// file with os.Open and immediately returns after json.Unmarshal — it never
// calls stripWorld(dir) or stripWorld(c.FilePath()). The world-bit-strip
// migration lives inside Update(), so a node that reads an existing config
// file at startup (the common case after first run) but does not write it
// again will keep whatever permission bits the prior install left behind
// (e.g. 0644 from an earlier build). The migration only takes effect once
// some code path triggers an Update.
//
// Pre-fix: a config file that exists with mode 0644 stays 0644 after Init().
// Post-fix: Init()'s else-branch (file exists) should call stripWorld(dir)
// and stripWorld(c.FilePath()) too, so the migration runs on every startup
// — at which point this test must flip to expect 0o640 (or whatever
// "no world bits" reduction of 0644 yields, here 0o640).
func TestAuditUnfixed_CRYP13_StripWorldNotCalledOnInit(t *testing.T) {
	type secretConf struct{ Seed string }
	dir := t.TempDir()

	// Reflect.TypeOf[secretConf]().Name() == "secretConf", and the file path
	// is dataDir/config/<TypeName>.json. We must pre-create both the dir
	// and the file with permissive bits to simulate a legacy on-disk layout.
	confDir := filepath.Join(dir, "config")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Force the perm bits — MkdirAll may be umask-clipped, so chmod after.
	if err := os.Chmod(confDir, 0o755); err != nil {
		t.Fatal(err)
	}
	confFile := filepath.Join(confDir, "secretConf.json")
	if err := os.WriteFile(confFile, []byte(`{"Seed":"legacy"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(confFile, 0o644); err != nil {
		t.Fatal(err)
	}

	c := config.New(secretConf{"default-seed"}, &dir)

	// --- Init() path: existing file → read branch now strips world bits
	// (review8 GV-H7); this assertion was flipped from documenting the bug. ---
	if err := c.Init(); err != nil {
		t.Fatalf("Init returned error: %v", err)
	}

	fi, err := os.Stat(confFile)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm&0o007 != 0 {
		t.Fatalf("GV-H7/CRYP-13: after Init() on pre-existing 0644 file, mode = %o, "+
			"world bits must be stripped", perm)
	}
	di, err := os.Stat(confDir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := di.Mode().Perm(); perm&0o007 != 0 {
		t.Fatalf("GV-H7/CRYP-13: after Init() on pre-existing 0755 dir, mode = %o, "+
			"world bits must be stripped", perm)
	}

	// --- Update() path: stripWorld IS wired up here, narrow positive check. ---
	if err := c.Update(func(t *secretConf) { t.Seed = "rotated" }); err != nil {
		t.Fatalf("Update returned error: %v", err)
	}

	fi, err = os.Stat(confFile)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm&0o007 != 0 {
		t.Fatalf("audit CRYP-13 (Update path): after Update() file mode = %o, world bits not stripped "+
			"(Update path is the one that DOES work today)", perm)
	}
	di, err = os.Stat(confDir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := di.Mode().Perm(); perm&0o007 != 0 {
		t.Fatalf("audit CRYP-13 (Update path): after Update() dir mode = %o, world bits not stripped", perm)
	}
}
