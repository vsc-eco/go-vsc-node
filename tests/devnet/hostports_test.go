package devnet

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckHostPorts(t *testing.T) {
	rangeFile := filepath.Join(t.TempDir(), "ip_local_port_range")
	if err := os.WriteFile(rangeFile, []byte("32768\t60999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	withOffset := func(off string) *Config {
		t.Setenv("DEVNET_PORT_OFFSET", off)
		return DefaultConfig()
	}

	for _, off := range []string{"", "5000", "12000"} {
		if err := checkHostPorts(withOffset(off).hostPorts(), rangeFile); err != nil {
			t.Errorf("offset %q: %v", off, err)
		}
	}

	err := checkHostPorts(withOffset("15000").hostPorts(), rangeFile)
	if err == nil {
		t.Fatal("offset 15000 puts the magi gql ports at 33080-33084, inside the range, and passed")
	}
	for _, want := range []string{"magi-5 gql 33084", "hive 33091", "32768-60999"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "p2p") {
		t.Errorf("p2p ports (26720+) are below the range and were reported: %v", err)
	}

	// A port on either edge of the range counts.
	for _, port := range []int{32768, 60999} {
		if checkHostPorts(map[string]int{"edge": port}, rangeFile) == nil {
			t.Errorf("port %d is inside 32768-60999 and passed", port)
		}
	}
	if err := checkHostPorts(map[string]int{"edge": 32767}, rangeFile); err != nil {
		t.Errorf("port 32767 is below the range: %v", err)
	}

	// No procfs (macOS hosts): nothing to check.
	if err := checkHostPorts(withOffset("15000").hostPorts(), filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Errorf("missing range file: %v", err)
	}
}

// Start refuses the ports before it does anything else. New does not: a Devnet
// that never publishes ports (the fake-drone tests in hive_ops_test.go point
// DronePort at an httptest server, whose port is ephemeral by design) is fine.
func TestStartRefusesEphemeralHostPorts(t *testing.T) {
	t.Setenv("DEVNET_PORT_OFFSET", "15000")
	cfg := DefaultConfig()
	if checkHostPorts(cfg.hostPorts(), ephemeralPortRangeFile) == nil {
		t.Skip("this host's ephemeral range does not cover offset 15000")
	}
	cfg.DataDir = filepath.Join(t.TempDir(), "devnet")
	// No docker on PATH: were the check missing, Start would fail at its first
	// docker call instead of booting a devnet on the refused ports.
	t.Setenv("PATH", "")
	d, err := New(cfg)
	if err != nil {
		t.Fatalf("New must not check host ports: %v", err)
	}
	err = d.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "ephemeral port range") {
		t.Fatalf("Start with offset 15000: err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "haf-data")); !os.IsNotExist(err) {
		t.Errorf("Start created HAF data dirs before refusing")
	}
}
