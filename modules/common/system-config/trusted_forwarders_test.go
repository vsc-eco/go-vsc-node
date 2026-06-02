package systemconfig_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	systemconfig "vsc-node/modules/common/system-config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TrustedForwarders is governance-managed: empty by default on all networks
// (so `call_as` is unreachable), and populated via the sysconfig override
// JSON when a forwarder contract is deployed and ratified.

func TestTrustedForwarders_DefaultsAreEmpty(t *testing.T) {
	cases := []struct {
		name string
		cfg  systemconfig.SystemConfig
	}{
		{"mainnet", systemconfig.MainnetConfig()},
		{"testnet", systemconfig.TestnetConfig()},
		{"devnet", systemconfig.DevnetConfig()},
		{"mocknet", systemconfig.MocknetConfig()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Empty(t, c.cfg.TrustedForwarders(),
				"%s: TrustedForwarders should default to empty so call_as is unreachable until governance adds a forwarder",
				c.name)
		})
	}
}

func TestTrustedForwarders_LoadOverrides(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sysconfig.json")

	overrides := map[string]any{
		"trustedForwarders": []string{
			"contract:vsc1DashForwarderXXXXXXXXXXXXXXXXXXX",
			"contract:vsc1LtcForwarderXXXXXXXXXXXXXXXXXXXX",
		},
	}
	data, err := json.Marshal(overrides)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	cfg := systemconfig.MainnetConfig()
	require.NoError(t, cfg.LoadOverrides(path))

	got := cfg.TrustedForwarders()
	assert.Len(t, got, 2)
	assert.Contains(t, got, "contract:vsc1DashForwarderXXXXXXXXXXXXXXXXXXX")
	assert.Contains(t, got, "contract:vsc1LtcForwarderXXXXXXXXXXXXXXXXXXXX")
}

func TestTrustedForwarders_OmittedOverridePreservesDefault(t *testing.T) {
	// An override file without trustedForwarders must NOT clobber the default.
	dir := t.TempDir()
	path := filepath.Join(dir, "sysconfig.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"netId":"some-net"}`), 0o600))

	cfg := systemconfig.MainnetConfig()
	require.NoError(t, cfg.LoadOverrides(path))

	// Default is empty, so this should remain empty.
	assert.Empty(t, cfg.TrustedForwarders())
	assert.Equal(t, "some-net", cfg.NetId())
}

func TestTrustedForwarders_ReturnsCopy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sysconfig.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"trustedForwarders":["contract:vsc1one"]}`), 0o600))

	cfg := systemconfig.MainnetConfig()
	require.NoError(t, cfg.LoadOverrides(path))

	got := cfg.TrustedForwarders()
	got[0] = "contract:vsc1tampered"

	// Internal state must not have been mutated.
	again := cfg.TrustedForwarders()
	assert.Equal(t, "contract:vsc1one", again[0],
		"TrustedForwarders must return a defensive copy so callers can't mutate internal state")
}
