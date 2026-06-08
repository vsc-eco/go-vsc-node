package config_test

// review8 GV-H7 — Config.Init() must strip world bits on a pre-existing config
// file, not only on Update(). The node identity config can hold the BLS seed,
// Hive WIF and libp2p private key; a config left world-readable (0644) by an
// earlier build stayed world-readable on a node that only reads it at startup,
// letting any local user read the keys.

import (
	"os"
	"path/filepath"
	"testing"

	"vsc-node/modules/config"
)

func TestReview8_GVH7_InitStripsWorldBitsOnExistingFile(t *testing.T) {
	type secretConf struct{ Seed string }
	dir := t.TempDir()

	confDir := filepath.Join(dir, "config")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		t.Fatal(err)
	}
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
	if err := c.Init(); err != nil {
		t.Fatalf("Init returned error: %v", err)
	}

	fi, err := os.Stat(confFile)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm&0o007 != 0 {
		t.Fatalf("GV-H7: after Init() on a pre-existing world-readable key file, mode = %o; world bits must be stripped", perm)
	}
	di, err := os.Stat(confDir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := di.Mode().Perm(); perm&0o007 != 0 {
		t.Fatalf("GV-H7: after Init(), config dir mode = %o; world bits must be stripped", perm)
	}
}
