package systemconfig

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

// DashMappingContractId defaults to empty on every network — Dash IS-login
// isn't deployed by default. When empty, magi's state-engine wiring leaves
// the trusted-forwarders list empty → call_as aborts unconditionally, which
// is the safe-default until operators explicitly set the contract id.
func TestDashMappingContractId_DefaultsAreEmpty(t *testing.T) {
	for _, c := range []struct {
		name string
		cfg  SystemConfig
	}{
		{"mainnet", MainnetConfig()},
		{"testnet", TestnetConfig()},
		{"devnet", DevnetConfig()},
		{"mocknet", MocknetConfig()},
	} {
		t.Run(c.name, func(t *testing.T) {
			assert.Empty(t, c.cfg.DashMappingContractId(),
				"%s: DashMappingContractId should default to empty so call_as is unreachable until the mapping is deployed + wired", c.name)
		})
	}
}

func TestDashMappingContractId_LoadOverrides(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sysconfig.json")
	if err := os.WriteFile(path, []byte(`{"dashMappingContractId":"vsc1BdashMappingExampleID"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := DevnetConfig()
	if err := cfg.LoadOverrides(path); err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, "vsc1BdashMappingExampleID", cfg.DashMappingContractId())
}

func TestDashMappingContractId_OmittedOverridePreservesDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sysconfig.json")
	// Override file with no DashMappingContractId entry — default remains empty.
	if err := os.WriteFile(path, []byte(`{"netId":"vsc-devnet"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := DevnetConfig()
	if err := cfg.LoadOverrides(path); err != nil {
		t.Fatal(err)
	}
	assert.Empty(t, cfg.DashMappingContractId())
}
