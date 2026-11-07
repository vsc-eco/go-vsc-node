package systemconfig

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStablePoolAt_HeightAndID(t *testing.T) {
	c := &config{stablePools: []StablePool{
		{ID: "vsc1Stable", FromHeight: 100, Amp: 100, Asset0: "hbd", Decimals0: 3, Asset1: "usdc", Decimals1: 6},
	}}
	if _, ok := c.StablePoolAt("vsc1Stable", 99); ok {
		t.Fatal("listed before its FromHeight")
	}
	p, ok := c.StablePoolAt("vsc1Stable", 100)
	if !ok || p.Amp != 100 || p.Asset1 != "usdc" {
		t.Fatalf("at FromHeight: %+v %v", p, ok)
	}
	if _, ok := c.StablePoolAt("vsc1Other", 1_000); ok {
		t.Fatal("another pool must not be stable")
	}
	if _, ok := c.StablePoolAt("", 1_000); ok {
		t.Fatal("empty id")
	}
}

func TestStablePools_EmptyOnMainnetAndTestnet(t *testing.T) {
	for name, sc := range map[string]SystemConfig{"mainnet": MainnetConfig(), "testnet": TestnetConfig(), "devnet": DevnetConfig()} {
		c := sc.(*config)
		if len(c.stablePools) != 0 {
			t.Fatalf("%s lists stable pools before any is deployed: %+v", name, c.stablePools)
		}
	}
}

func writeOverride(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sysconfig.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const stableOverride = `{"stablePools":[{"id":"vsc1Stable","fromHeight":0,"amp":100,"asset0":"hbd","decimals0":3,"asset1":"usdc","decimals1":6}]}`

// Consensus input: an operator file must not list (or unlist) a stable pool
// on mainnet or testnet; devnet tests set it this way.
func TestStablePools_OverrideBakedExceptDevnet(t *testing.T) {
	for name, sc := range map[string]SystemConfig{"mainnet": MainnetConfig(), "testnet": TestnetConfig()} {
		if err := sc.LoadOverrides(writeOverride(t, stableOverride)); err != nil {
			t.Fatal(err)
		}
		if _, ok := sc.StablePoolAt("vsc1Stable", 1); ok {
			t.Fatalf("%s took a stable pool from -sysconfig", name)
		}
	}
	dev := DevnetConfig()
	if err := dev.LoadOverrides(writeOverride(t, stableOverride)); err != nil {
		t.Fatal(err)
	}
	p, ok := dev.StablePoolAt("vsc1Stable", 1)
	if !ok || p.Amp != 100 || p.Decimals0 != 3 || p.Decimals1 != 6 || p.Asset0 != "hbd" {
		t.Fatalf("devnet override: %+v %v", p, ok)
	}
}
