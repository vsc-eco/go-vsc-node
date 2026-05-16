package config_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"vsc-node/modules/config"
)

// review2 CRITICAL #5 — the identity config (BLS seed, Hive active WIF,
// libp2p private key) was written world-readable (0644) in a world-readable
// dir (0755). Persisted node config must be owner-only.
func TestConfigFilePermissionsAreRestrictive(t *testing.T) {
	type secretConf struct{ Seed string }
	dir := t.TempDir()
	c := config.New(secretConf{"super-secret-bls-seed"}, &dir)
	if err := c.Init(); err != nil {
		t.Fatal(err)
	}
	fp := c.FilePath()

	fi, err := os.Stat(fp)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("config file %s mode = %o, want 0600 (holds BLS seed / Hive WIF / libp2p key)", fp, perm)
	}

	di, err := os.Stat(filepath.Dir(fp))
	if err != nil {
		t.Fatal(err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Fatalf("config dir %s mode = %o, want 0700", filepath.Dir(fp), perm)
	}
}

func TestBasic(t *testing.T) {
	type conf struct {
		A uint
		B string
	}
	c := config.New(conf{1, "hi"}, nil)
	err := c.Init()
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Start().Await(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	err = c.Stop()
	if err != nil {
		t.Fatal(err)
	}
}
