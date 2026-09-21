package systemconfig

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPendulumPoolWhitelist_AccessorReturnsCopy(t *testing.T) {
	c := &config{pendulumPoolWhitelist: []string{"vsc1A", "vsc1B"}}
	got := c.PendulumPoolWhitelistAt(0)
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
	got := c.PendulumPoolWhitelistAt(0)
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
	if w := c.PendulumPoolWhitelistAt(0); len(w) != 0 {
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
	got := c.PendulumPoolWhitelistAt(0)
	if len(got) != 1 || got[0] != "vsc1Default" {
		t.Fatalf("absent key must preserve default; got %v", got)
	}
}

// ─── Staged expansion (activation height) ────────────────────────────────────
//
// The whitelist is summed into the pendulum geometry (P = Σ HBD-side reserve
// over whitelisted pools, V = 2P, s = V/E), so it decides the fee split for
// EVERY pool, not just the one being added. If an expansion applied the moment
// each operator upgraded, witnesses on either side of a rolling upgrade would
// compute different s for the same block and diverge on ordinary swaps. Hence
// the staged list + activation height, and hence these tests.

func stagedConfig() *config {
	return &config{
		pendulumPoolWhitelist:     []string{"vsc1Pool1", "vsc1Pool2"},
		pendulumPoolWhitelistV2:   []string{"vsc1Pool1", "vsc1Pool2", "vsc1Pool3"},
		pendulumWhitelistV2Height: 1000,
	}
}

func TestPendulumWhitelist_BeforeActivationHeight_UsesBaseList(t *testing.T) {
	c := stagedConfig()
	for _, h := range []uint64{0, 1, 999} {
		got := c.PendulumPoolWhitelistAt(h)
		if len(got) != 2 {
			t.Fatalf("height %d: expected the pre-expansion list, got %v", h, got)
		}
	}
}

func TestPendulumWhitelist_AtAndAfterActivationHeight_UsesV2(t *testing.T) {
	c := stagedConfig()
	for _, h := range []uint64{1000, 1001, 1 << 40} {
		got := c.PendulumPoolWhitelistAt(h)
		if len(got) != 3 || got[2] != "vsc1Pool3" {
			t.Fatalf("height %d: expected the expanded list, got %v", h, got)
		}
	}
}

// The boundary is the whole point: one block either side must not disagree
// about which list applies.
func TestPendulumWhitelist_ActivationBoundaryIsExact(t *testing.T) {
	c := stagedConfig()
	if len(c.PendulumPoolWhitelistAt(999)) != 2 {
		t.Fatal("block 999 must still use the base list")
	}
	if len(c.PendulumPoolWhitelistAt(1000)) != 3 {
		t.Fatal("block 1000 must use the expanded list")
	}
}

func TestPendulumWhitelist_ZeroHeightActivatesImmediately(t *testing.T) {
	c := stagedConfig()
	c.pendulumWhitelistV2Height = 0
	if len(c.PendulumPoolWhitelistAt(0)) != 3 {
		t.Fatal("height 0 means no gate; testnet/devnet expect immediate application")
	}
}

func TestPendulumWhitelist_NoStagedExpansion_AlwaysBaseList(t *testing.T) {
	c := &config{pendulumPoolWhitelist: []string{"vsc1Pool1"}}
	if got := c.PendulumPoolWhitelistAt(1 << 40); len(got) != 1 {
		t.Fatalf("a network with no staged expansion must ignore height; got %v", got)
	}
}

// An operator who set the list deliberately gets exactly that, at every
// height — the staged rollout coordinates DEFAULTS, it does not override
// explicit intent.
func TestPendulumWhitelist_OperatorOverrideBeatsStagedExpansion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sysconfig.json")
	if err := os.WriteFile(path, []byte(`{"pendulumPoolWhitelist":["vsc1Only"]}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	c := stagedConfig()
	if err := c.LoadOverrides(path); err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, h := range []uint64{0, 999, 1000, 1 << 40} {
		got := c.PendulumPoolWhitelistAt(h)
		if len(got) != 1 || got[0] != "vsc1Only" {
			t.Fatalf("height %d: override must win, got %v", h, got)
		}
	}
}

func TestPendulumWhitelist_AccessorReturnsCopyPostActivation(t *testing.T) {
	c := stagedConfig()
	got := c.PendulumPoolWhitelistAt(2000)
	got[0] = "MUTATED"
	if c.pendulumPoolWhitelistV2[0] != "vsc1Pool1" {
		t.Fatal("accessor must copy the V2 slice too; internal slice was mutated")
	}
}

// Mainnet ships the real staged expansion; guard the shape so an edit that
// drops the gate (or edits the base list in place) fails loudly here.
func TestMainnetConfig_WhitelistExpansionIsStaged(t *testing.T) {
	c := MainnetConfig().(*config)
	if c.pendulumWhitelistV2Height == 0 {
		t.Fatal("mainnet expansion must be gated on an activation height")
	}
	if len(c.pendulumPoolWhitelistV2) <= len(c.pendulumPoolWhitelist) {
		t.Fatal("V2 must be a superset; expansions are staged, not edited in place")
	}
	before := c.PendulumPoolWhitelistAt(c.pendulumWhitelistV2Height - 1)
	after := c.PendulumPoolWhitelistAt(c.pendulumWhitelistV2Height)
	if len(before) != len(c.pendulumPoolWhitelist) || len(after) != len(c.pendulumPoolWhitelistV2) {
		t.Fatalf("mainnet gate not wired: before=%v after=%v", before, after)
	}
}
