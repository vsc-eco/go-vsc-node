package systemconfig

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPendulumPoolWhitelist_DefaultsAreEmpty(t *testing.T) {
	for name, c := range map[string]SystemConfig{
		"mainnet": MainnetConfig(),
		"testnet": TestnetConfig(),
		"devnet":  DevnetConfig(),
		"mocknet": MocknetConfig(),
	} {
		if w := c.PendulumPoolWhitelist(); len(w) != 0 {
			t.Errorf("%s: expected empty whitelist by default; got %v", name, w)
		}
	}
}

func TestPendulumPoolWhitelist_AccessorReturnsCopy(t *testing.T) {
	c := &config{pendulumPoolWhitelist: []string{"vsc1A", "vsc1B"}}
	got := c.PendulumPoolWhitelist()
	got[0] = "MUTATED"
	if c.pendulumPoolWhitelist[0] != "vsc1A" {
		t.Fatal("accessor must return a copy; internal slice was mutated")
	}
}

func TestLoadOverrides_PendulumPoolWhitelist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sysconfig.json")
	const body = `{"pendulumPoolWhitelist":["vsc1Pool1","vsc1Pool2"]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	c := &config{}
	if err := c.LoadOverrides(path); err != nil {
		t.Fatalf("load: %v", err)
	}
	got := c.PendulumPoolWhitelist()
	if len(got) != 2 || got[0] != "vsc1Pool1" || got[1] != "vsc1Pool2" {
		t.Fatalf("unexpected whitelist: %v", got)
	}
}

func TestLoadOverrides_PendulumPoolWhitelist_EmptyArrayReplacesDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sysconfig.json")
	if err := os.WriteFile(path, []byte(`{"pendulumPoolWhitelist":[]}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	c := &config{pendulumPoolWhitelist: []string{"vsc1Default"}}
	if err := c.LoadOverrides(path); err != nil {
		t.Fatalf("load: %v", err)
	}
	if w := c.PendulumPoolWhitelist(); len(w) != 0 {
		t.Fatalf("expected explicit empty array to clear default; got %v", w)
	}
}

func TestLoadOverrides_PendulumPoolWhitelist_AbsentKeyKeepsDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sysconfig.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	c := &config{pendulumPoolWhitelist: []string{"vsc1Default"}}
	if err := c.LoadOverrides(path); err != nil {
		t.Fatalf("load: %v", err)
	}
	got := c.PendulumPoolWhitelist()
	if len(got) != 1 || got[0] != "vsc1Default" {
		t.Fatalf("absent key must preserve default; got %v", got)
	}
}
