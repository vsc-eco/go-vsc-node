package systemconfig

import (
	"os"
	"path/filepath"
	"testing"

	"vsc-node/modules/common/params"
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

// ─── Staged additions: swap gate vs collateral ───────────────────────────────
//
// "Whitelisted" hides two separate privileges:
//
//   swap       — may the pool call the pendulum at all (else every swap aborts)
//   collateral — does its HBD reserve enter P, hence V = 2P and s = V/E, which
//                set the LP/node fee split on EVERY pool
//
// A community pool needs the first and must not get the second: P trusts a
// pool's self-reported r0, so an owner who can ship a code update could report
// an inflated reserve and move fees away from LPs on the DAO pools.
// docs/incentive-pendulum.md restricts V/P/s to hive:vsc.dao pools for exactly
// this reason. These tests pin that the two lists cannot drift back together.

func stagedConfig() *config {
	return &config{
		pendulumPoolWhitelist: []string{"vsc1Dao1", "vsc1Dao2"},
		pendulumPoolAdditions: []PendulumPoolAddition{
			{ID: "vsc1Community", FromHeight: 1000, Collateral: false},
			{ID: "vsc1DaoLater", FromHeight: 2000, Collateral: true},
		},
	}
}

func has(list []string, id string) bool {
	for _, v := range list {
		if v == id {
			return true
		}
	}
	return false
}

func TestPendulumPools_AdditionInactiveBeforeItsHeight(t *testing.T) {
	c := stagedConfig()
	for _, h := range []uint64{0, 1, 999} {
		if has(c.PendulumPoolWhitelistAt(h), "vsc1Community") {
			t.Fatalf("height %d: addition must not be live before FromHeight", h)
		}
	}
}

func TestPendulumPools_SwapOnlyPoolSwapsButNeverCounts(t *testing.T) {
	c := stagedConfig()
	for _, h := range []uint64{1000, 1001, 1 << 40} {
		if !has(c.PendulumPoolWhitelistAt(h), "vsc1Community") {
			t.Fatalf("height %d: swap-only pool must be able to swap", h)
		}
		if has(c.PendulumCollateralPoolsAt(h), "vsc1Community") {
			t.Fatalf("height %d: swap-only pool must NEVER enter collateral", h)
		}
	}
}

func TestPendulumPools_CollateralAdditionEntersBothAtItsHeight(t *testing.T) {
	c := stagedConfig()
	if has(c.PendulumCollateralPoolsAt(1999), "vsc1DaoLater") {
		t.Fatal("collateral addition must not be live before FromHeight")
	}
	if !has(c.PendulumCollateralPoolsAt(2000), "vsc1DaoLater") {
		t.Fatal("collateral addition must be live at FromHeight")
	}
	if !has(c.PendulumPoolWhitelistAt(2000), "vsc1DaoLater") {
		t.Fatal("anything that counts as collateral must also be allowed to swap")
	}
}

// The invariant that matters: you can never end up backing the fee split with
// a pool that isn't even allowed to trade.
func TestPendulumPools_CollateralIsAlwaysASubsetOfSwap(t *testing.T) {
	c := stagedConfig()
	for _, h := range []uint64{0, 999, 1000, 1999, 2000, 1 << 40} {
		swap := c.PendulumPoolWhitelistAt(h)
		for _, id := range c.PendulumCollateralPoolsAt(h) {
			if !has(swap, id) {
				t.Fatalf("height %d: %s counts as collateral but cannot swap", h, id)
			}
		}
	}
}

func TestPendulumPools_BaseListCarriesBothPrivileges(t *testing.T) {
	c := stagedConfig()
	for _, id := range []string{"vsc1Dao1", "vsc1Dao2"} {
		if !has(c.PendulumPoolWhitelistAt(0), id) || !has(c.PendulumCollateralPoolsAt(0), id) {
			t.Fatalf("%s: base list must swap and count from genesis", id)
		}
	}
}

func TestPendulumPools_ZeroFromHeightIsImmediate(t *testing.T) {
	c := &config{pendulumPoolAdditions: []PendulumPoolAddition{{ID: "vsc1Now", Collateral: true}}}
	if !has(c.PendulumCollateralPoolsAt(0), "vsc1Now") {
		t.Fatal("FromHeight 0 means immediate — testnet/devnet rely on this")
	}
}

// A pool named in both the base list and an addition must not be summed twice
// into P.
func TestPendulumPools_DeduplicatesAcrossBaseAndAdditions(t *testing.T) {
	c := &config{
		pendulumPoolWhitelist: []string{"vsc1Dup"},
		pendulumPoolAdditions: []PendulumPoolAddition{{ID: "vsc1Dup", Collateral: true}},
	}
	if got := c.PendulumCollateralPoolsAt(0); len(got) != 1 {
		t.Fatalf("expected one entry, got %v — P would double-count", got)
	}
	if got := c.PendulumPoolWhitelistAt(0); len(got) != 1 {
		t.Fatalf("expected one entry, got %v", got)
	}
}

func TestPendulumPools_OperatorOverrideBeatsAdditions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sysconfig.json")
	if err := os.WriteFile(path, []byte(`{"pendulumPoolWhitelist":["vsc1Only"]}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	c := stagedConfig()
	if err := c.LoadOverrides(path); err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, h := range []uint64{0, 1000, 2000, 1 << 40} {
		swap, coll := c.PendulumPoolWhitelistAt(h), c.PendulumCollateralPoolsAt(h)
		if len(swap) != 1 || swap[0] != "vsc1Only" {
			t.Fatalf("height %d: override must win for swap, got %v", h, swap)
		}
		// An override grants both privileges — the pre-split behaviour the
		// devnet harness (setPendulumWhitelistAndRestart) depends on.
		if len(coll) != 1 || coll[0] != "vsc1Only" {
			t.Fatalf("height %d: override must win for collateral, got %v", h, coll)
		}
	}
}

func TestPendulumPools_AccessorsReturnCopies(t *testing.T) {
	c := stagedConfig()
	c.PendulumPoolWhitelistAt(0)[0] = "MUTATED"
	c.PendulumCollateralPoolsAt(0)[0] = "MUTATED"
	if c.pendulumPoolWhitelist[0] != "vsc1Dao1" {
		t.Fatal("accessors must copy; internal slice was mutated")
	}
}

// Mainnet: LASSECASH trades from the height but the collateral set stays
// exactly the two DAO pools, so P — and therefore the fee split on HBD:HIVE
// and BTC:HBD — does not move at activation.
func TestMainnetConfig_LassecashIsSwapOnly(t *testing.T) {
	c := MainnetConfig().(*config)
	const lasse = "vsc1BrBFAwZ3Mr8L4ijRqT9RPEPvhK9FWDaYSr"
	h := params.PENDULUM_WHITELIST_V2_HEIGHT
	if h == 0 {
		t.Fatal("mainnet addition must be gated on an activation height")
	}

	if has(c.PendulumPoolWhitelistAt(h-1), lasse) {
		t.Fatal("LASSECASH must not swap before the activation height")
	}
	if !has(c.PendulumPoolWhitelistAt(h), lasse) {
		t.Fatal("LASSECASH must swap from the activation height")
	}
	for _, at := range []uint64{h - 1, h, 1 << 40} {
		if has(c.PendulumCollateralPoolsAt(at), lasse) {
			t.Fatalf("height %d: LASSECASH must never count as collateral", at)
		}
		coll := c.PendulumCollateralPoolsAt(at)
		if len(coll) != 2 ||
			!has(coll, "vsc1BoaniA5HW56GuQy6pVdoZfMcVaaDfnC8kp") ||
			!has(coll, "vsc1BVb95YKRHAEy24XgRSaW4L6d9vB88AdwjM") {
			t.Fatalf("height %d: collateral set must be exactly the two DAO pools, got %v", at, coll)
		}
	}
}
