package devnet

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDevnetSetup is an integration test that spins up a complete
// devnet environment: a Hive testnet (HAF), MongoDB, 5 Magi nodes,
// and optionally deploys a test contract. This test is slow and
// requires Docker.
//
// Run with:
//
//	go test -v -run TestDevnetSetup -timeout 20m ./tests/devnet/
//
// To keep containers running after the test (for debugging):
//
//	DEVNET_KEEP=1 go test -v -run TestDevnetSetup -timeout 20m ./tests/devnet/
func TestDevnetSetup(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}
	requireDocker(t)

	cfg := DefaultConfig()
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}

	d, err := New(cfg)
	if err != nil {
		t.Fatalf("creating devnet: %v", err)
	}
	t.Cleanup(func() {
		if err := d.Stop(); err != nil {
			t.Logf("warning: stop failed: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	if err := d.Start(ctx); err != nil {
		// Dump logs on failure for easier debugging
		for i := 1; i <= cfg.Nodes; i++ {
			logs, _ := d.Logs(ctx, fmt.Sprintf("magi-%d", i))
			if logs != "" {
				t.Logf("magi-%d logs:\n%s", i, truncateLogs(logs, 30))
			}
		}
		hafLogs, _ := d.Logs(ctx, "haf")
		if hafLogs != "" {
			t.Logf("haf logs:\n%s", truncateLogs(hafLogs, 30))
		}
		t.Fatalf("starting devnet: %v", err)
	}

	t.Logf("Devnet running:")
	t.Logf("  Hive RPC: %s", d.HiveRPCEndpoint())
	t.Logf("  MongoDB:  %s", d.MongoURI())
	for i := 1; i <= cfg.Nodes; i++ {
		t.Logf("  magi-%d:   %s", i, d.GQLEndpoint(i))
	}
	t.Logf("  Data dir: %s", d.DataDir())

	// Verify we can reach the Hive RPC endpoint
	t.Run("hive_rpc_reachable", func(t *testing.T) {
		out, err := exec.CommandContext(ctx,
			"curl", "-sf", "-o", "/dev/null", "-w", "%{http_code}",
			d.HiveRPCEndpoint(),
		).Output()
		if err != nil {
			t.Skipf("hive RPC not reachable (may need port forwarding): %v", err)
		}
		t.Logf("Hive RPC responded with HTTP %s", string(out))
	})
}

// TestDeployCallTss builds the call-tss contract, starts a devnet,
// and deploys it. This is the pattern most TSS tests will follow.
//
// Run with:
//
//	go test -v -run TestDeployCallTss -timeout 25m ./tests/devnet/
func TestDeployCallTss(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet contract deploy test in short mode")
	}
	requireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	// Build the call-tss contract
	wasmPath, err := BuildCallTssContract(ctx)
	if err != nil {
		t.Fatalf("building call-tss contract: %v", err)
	}

	cfg := DefaultConfig()
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}

	d, err := New(cfg)
	if err != nil {
		t.Fatalf("creating devnet: %v", err)
	}
	t.Cleanup(func() { d.Stop() })

	if err := d.Start(ctx); err != nil {
		t.Fatalf("starting devnet: %v", err)
	}

	contractId, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath: wasmPath,
		Name:     "call-tss",
	})
	if err != nil {
		t.Fatalf("deploying contract: %v", err)
	}
	t.Logf("Contract deployed: %s", contractId)
}

// TestEnvFileGeneration validates that the .env file is generated with
// the correct values. Does not require Docker.
func TestEnvFileGeneration(t *testing.T) {
	cfg := DefaultConfig()
	tmpDir := t.TempDir()
	envPath := filepath.Join(tmpDir, ".env")

	hafDir := filepath.Join(tmpDir, "haf")
	devnetDir := filepath.Join(tmpDir, "devnet")

	if err := writeEnvFile(cfg, hafDir, devnetDir, envPath); err != nil {
		t.Fatalf("writeEnvFile: %v", err)
	}

	data, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("reading env file: %v", err)
	}
	content := string(data)

	checks := []string{
		"SOURCE_DIR=",
		"HAF_IMAGE=registry.gitlab.syncad.com/hive/haf/testnet",
		"MONGO_IMAGE=mongo:8.0.17",
		fmt.Sprintf("HAF_DATA_DIR=%s", hafDir),
		fmt.Sprintf("DEVNET_DATA_DIR=%s", devnetDir),
		"HIVE_PORT=18091",
		"MONGO_PORT=18057",
		"DEVNET_NODES=5",
		"GENESIS_NODE=5",
		"LOG_LEVEL=error,tss=trace",
	}
	for _, check := range checks {
		if !strings.Contains(content, check) {
			t.Errorf("env file missing %q", check)
		}
	}

	t.Logf("Generated .env:\n%s", content)
}

// TestComposeFileExists verifies the static docker-compose.yml is present
// and contains expected service definitions.
func TestComposeFileExists(t *testing.T) {
	path := composeFilePath()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("docker-compose.yml not found at %s: %v", path, err)
	}
	content := string(data)

	checks := []string{
		"${HAF_IMAGE}",
		"${HAF_DATA_DIR}",
		"${DEVNET_DATA_DIR}",
		"${HIVE_PORT}",
		"${MONGO_PORT}",
		"${DEVNET_NODES}",
		"${GENESIS_NODE}",
		"${SOURCE_DIR}",
		"devnet-setup:",
		"genesis-elector:",
		"contract-deployer:",
		"http://haf:8091",
		"/dns4/magi-?",
	}
	for _, check := range checks {
		if !strings.Contains(content, check) {
			t.Errorf("docker-compose.yml missing %q", check)
		}
	}
}

// TestNodesOverride validates the generated nodes override file.
func TestNodesOverride(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Nodes = 7

	tmpDir := t.TempDir()
	devnetDir := filepath.Join(tmpDir, "devnet")
	outPath := filepath.Join(tmpDir, "nodes.yml")

	if err := writeNodesOverride(cfg, devnetDir, outPath); err != nil {
		t.Fatalf("writeNodesOverride: %v", err)
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)

	for i := 1; i <= 7; i++ {
		name := fmt.Sprintf("magi-%d:", i)
		if !strings.Contains(content, name) {
			t.Errorf("override missing %q", name)
		}
	}
	if strings.Contains(content, "magi-8:") {
		t.Error("override should not contain magi-8")
	}

	t.Logf("Generated nodes override (%d bytes):\n%s", len(data), content)
}

// TestHAFDataDirs validates the HAF directory structure creation.
func TestHAFDataDirs(t *testing.T) {
	tmpDir := t.TempDir()
	hafDir := filepath.Join(tmpDir, "haf")

	if err := createHAFDataDirs(hafDir); err != nil {
		t.Fatalf("createHAFDataDirs: %v", err)
	}

	expectedFiles := []string{
		"config.ini",
		"haf_db_store/haf_postgresql_conf.d/pgtune.conf",
	}
	for _, f := range expectedFiles {
		path := filepath.Join(hafDir, f)
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("expected file missing: %s", f)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("file is empty: %s", f)
		}
	}

	expectedDirs := []string{
		"blockchain",
		"haf_db_store/pgdata/pg_wal",
		"haf_db_store/tablespace",
		"logs/postgresql",
	}
	for _, d := range expectedDirs {
		path := filepath.Join(hafDir, d)
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			t.Errorf("expected directory missing: %s", d)
		}
	}
}

func requireDocker(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not found in PATH")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon not running")
	}
}
