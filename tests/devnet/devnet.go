package devnet

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Devnet manages a complete devnet test environment including a Hive
// testnet (HAF), MongoDB, and multiple VSC (Magi) nodes orchestrated
// via Docker Compose.
type Devnet struct {
	cfg          *Config
	dataDir      string
	hafDataDir   string
	devnetDir    string
	composeFile  string
	overrideFile string
	envFile      string
	projectName  string
	started      bool
}

// New creates a new Devnet instance. If cfg is nil, DefaultConfig() is used.
func New(cfg *Config) (*Devnet, error) {
	if cfg == nil {
		cfg = DefaultConfig()
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	d := &Devnet{cfg: cfg}

	if cfg.DataDir != "" {
		d.dataDir = cfg.DataDir
	} else {
		// Default: .devnet/ inside the repo root, with a random suffix
		// to allow parallel runs.
		b := make([]byte, 4)
		rand.Read(b)
		d.dataDir = filepath.Join(cfg.SourceDir, ".devnet", hex.EncodeToString(b))
	}
	if err := os.MkdirAll(d.dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating data dir: %w", err)
	}

	d.hafDataDir = filepath.Join(d.dataDir, "haf-data")
	d.devnetDir = filepath.Join(d.dataDir, "devnet-data")
	d.composeFile = composeFilePath()
	d.overrideFile = filepath.Join(d.dataDir, "docker-compose.nodes.yml")
	d.envFile = filepath.Join(d.dataDir, ".env")

	if cfg.ProjectName != "" {
		d.projectName = cfg.ProjectName
	} else {
		b := make([]byte, 4)
		rand.Read(b)
		d.projectName = "devnet-test-" + hex.EncodeToString(b)
	}

	return d, nil
}

// Start brings up the complete devnet environment. The sequence is:
//  1. Create HAF data directories and write config files
//  2. Write .env file for docker compose variable substitution
//  3. Build the VSC devnet Docker image
//  4. Start HAF + MongoDB and wait for healthy
//  5. Run devnet-setup (create Hive accounts, node configs, stake)
//  6. Start all magi nodes
//  7. Stop the genesis node, run genesis-elector, restart it
func (d *Devnet) Start(ctx context.Context) error {
	if d.started {
		return fmt.Errorf("devnet already started")
	}

	log.Printf("[devnet] project=%s dir=%s", d.projectName, d.dataDir)

	// Step 1: HAF data directories
	log.Printf("[devnet] creating HAF data directories...")
	if err := createHAFDataDirs(d.hafDataDir); err != nil {
		return fmt.Errorf("creating HAF data dirs: %w", err)
	}
	if err := os.MkdirAll(d.devnetDir, 0o777); err != nil {
		return fmt.Errorf("creating devnet dir: %w", err)
	}
	// Ensure the devnet data dir is world-writable so the container's
	// app user (uid != host uid) can create node data directories.
	os.Chmod(d.devnetDir, 0o777)

	// Step 2: write drone config, .env, and nodes override
	log.Printf("[devnet] writing drone config...")
	droneConfigPath, err := writeDroneConfig(d.dataDir)
	if err != nil {
		return fmt.Errorf("writing drone config: %w", err)
	}
	log.Printf("[devnet] writing %s", d.envFile)
	if err := writeEnvFile(d.cfg, d.hafDataDir, d.devnetDir, droneConfigPath, d.envFile); err != nil {
		return fmt.Errorf("writing env file: %w", err)
	}
	if err := writeSysConfigOverrides(d.cfg, d.devnetDir); err != nil {
		return fmt.Errorf("writing sysconfig overrides: %w", err)
	}
	log.Printf("[devnet] writing %s", d.overrideFile)
	if err := writeNodesOverride(d.cfg, d.devnetDir, d.overrideFile); err != nil {
		return fmt.Errorf("writing nodes override: %w", err)
	}

	// Step 3a: build old-code image if multi-version testing is configured
	if d.cfg.OldCodeSourceDir != "" && len(d.cfg.OldCodeNodes) > 0 {
		log.Printf("[devnet] building old-code image...")
		if err := d.BuildOldCodeImage(ctx); err != nil {
			return fmt.Errorf("building old-code image: %w", err)
		}
	}

	// Step 3b: build image
	log.Printf("[devnet] building devnet image (this may take a while)...")
	if err := d.compose(ctx, "build"); err != nil {
		return fmt.Errorf("building image: %w", err)
	}

	// Step 4: start infrastructure
	log.Printf("[devnet] starting HAF and MongoDB...")
	if err := d.compose(ctx, "up", "-d", "haf", "db"); err != nil {
		return fmt.Errorf("starting HAF+DB: %w", err)
	}

	log.Printf("[devnet] waiting for MongoDB...")
	if err := d.waitForService(ctx, "db", 1*time.Minute); err != nil {
		return fmt.Errorf("MongoDB health check: %w", err)
	}

	log.Printf("[devnet] waiting for HAF (hived + postgres)...")
	if err := d.waitForService(ctx, "haf", 5*time.Minute); err != nil {
		return fmt.Errorf("HAF health check: %w", err)
	}
	log.Printf("[devnet] HAF is healthy")

	// Step 5: start hafah API stack (hafah-install → pgbouncer → hafah-postgrest → drone)
	// Drone must be running before devnet-setup, which uses it as the Hive API.
	log.Printf("[devnet] starting hafah API stack (hafah-install, pgbouncer, hafah-postgrest, drone)...")
	if err := d.compose(ctx, "up", "-d", "drone"); err != nil {
		return fmt.Errorf("starting hafah stack: %w", err)
	}
	log.Printf("[devnet] waiting for drone API router...")
	if err := d.waitForService(ctx, "drone", 5*time.Minute); err != nil {
		return fmt.Errorf("drone health check: %w", err)
	}
	log.Printf("[devnet] hafah API stack is ready")

	// Step 6: devnet-setup (writes node configs with drone as Hive API URL)
	log.Printf("[devnet] running devnet-setup...")
	if err := d.compose(ctx, "run", "--rm", "devnet-setup"); err != nil {
		return fmt.Errorf("devnet-setup: %w", err)
	}

	log.Printf("[devnet] starting %d magi nodes...", d.cfg.Nodes)
	names := make([]string, d.cfg.Nodes)
	for i := range names {
		names[i] = fmt.Sprintf("magi-%d", i+1)
	}
	if err := d.compose(ctx, append([]string{"up", "-d"}, names...)...); err != nil {
		return fmt.Errorf("starting magi nodes: %w", err)
	}

	// Give nodes time to initialize and connect to each other
	log.Printf("[devnet] waiting for nodes to initialize...")
	time.Sleep(10 * time.Second)

	// Step 7: genesis election
	genesisName := fmt.Sprintf("magi-%d", d.cfg.GenesisNode)
	log.Printf("[devnet] stopping %s for genesis election...", genesisName)
	if err := d.compose(ctx, "stop", genesisName); err != nil {
		return fmt.Errorf("stopping genesis node: %w", err)
	}

	log.Printf("[devnet] running genesis-elector...")
	if err := d.compose(ctx, "run", "--rm", "genesis-elector"); err != nil {
		return fmt.Errorf("genesis-elector: %w", err)
	}

	log.Printf("[devnet] restarting %s...", genesisName)
	if err := d.compose(ctx, "start", genesisName); err != nil {
		return fmt.Errorf("restarting genesis node: %w", err)
	}

	// Step 8: fund accounts for contract deployment (optional)
	if !d.cfg.SkipFunding {
		if err := d.fundAccounts(); err != nil {
			return fmt.Errorf("funding accounts: %w", err)
		}
	} else {
		log.Printf("[devnet] skipping account funding (SkipFunding=true)")
	}

	d.started = true
	log.Printf("[devnet] devnet is running")
	log.Printf("[devnet]   Hive RPC: %s", d.HiveRPCEndpoint())
	log.Printf("[devnet]   Drone:    %s", d.DroneEndpoint())
	log.Printf("[devnet]   MongoDB:  %s", d.MongoURI())
	for i := 1; i <= d.cfg.Nodes; i++ {
		log.Printf("[devnet]   magi-%d GQL: %s", i, d.GQLEndpoint(i))
	}
	return nil
}

// Stop tears down the devnet environment.
func (d *Devnet) Stop() error {
	if d.cfg.KeepRunning {
		log.Printf("[devnet] KeepRunning=true, skipping teardown")
		log.Printf("[devnet]   project: %s", d.projectName)
		log.Printf("[devnet]   data:    %s", d.dataDir)
		return nil
	}

	log.Printf("[devnet] tearing down...")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := d.compose(ctx, "down", "-v", "--remove-orphans"); err != nil {
		log.Printf("[devnet] warning: compose down failed: %v", err)
	}

	// The HAF container runs as a different uid (postgres, hived) so
	// some files under haf-data/ will be owned by those uids.  Use
	// sudo rm to clean them up.
	if d.cfg.DataDir == "" {
		cmd := exec.Command("sudo", "rm", "-rf", d.dataDir)
		if err := cmd.Run(); err != nil {
			// Fall back to regular rm (works if no root-owned files remain)
			if err2 := os.RemoveAll(d.dataDir); err2 != nil {
				log.Printf("[devnet] warning: failed to remove %s: %v", d.dataDir, err2)
			}
		}
	}

	d.started = false
	log.Printf("[devnet] stopped")
	return nil
}

// GQLEndpoint returns the GraphQL endpoint URL for the given node (1-indexed).
func (d *Devnet) GQLEndpoint(node int) string {
	return fmt.Sprintf("http://localhost:%d/api/v1/graphql", d.cfg.GQLBasePort+node-1)
}

// HiveRPCEndpoint returns the Hive JSON-RPC endpoint URL.
func (d *Devnet) HiveRPCEndpoint() string {
	return fmt.Sprintf("http://localhost:%d", d.cfg.HivePort)
}

// MongoURI returns the MongoDB connection URI.
func (d *Devnet) MongoURI() string {
	return fmt.Sprintf("mongodb://localhost:%d", d.cfg.MongoPort)
}

// DroneEndpoint returns the drone API router endpoint URL.
func (d *Devnet) DroneEndpoint() string {
	return fmt.Sprintf("http://localhost:%d", d.cfg.DronePort)
}

// ProjectName returns the docker compose project name.
func (d *Devnet) ProjectName() string {
	return d.projectName
}

// DataDir returns the root data directory path.
func (d *Devnet) DataDir() string {
	return d.dataDir
}

// ComposeFile returns the path to docker-compose.yml.
func (d *Devnet) ComposeFile() string {
	return d.composeFile
}

// StopNode stops a single magi node container (1-indexed).
func (d *Devnet) StopNode(ctx context.Context, node int) error {
	name := fmt.Sprintf("magi-%d", node)
	log.Printf("[devnet] stopping %s", name)
	return d.compose(ctx, "stop", name)
}

// StartNode starts a previously stopped magi node container (1-indexed).
func (d *Devnet) StartNode(ctx context.Context, node int) error {
	name := fmt.Sprintf("magi-%d", node)
	log.Printf("[devnet] starting %s", name)
	return d.compose(ctx, "start", name)
}

// BuildOldCodeImage builds a Docker image from the old code source
// directory. It writes a temporary Dockerfile that:
//  1. Runs gqlgen generate (required by older code)
//  2. Builds the magid binary
//  3. Includes iptables for partition testing
//
// The image is tagged so it can be referenced by old-code node entries
// in the compose file.
func (d *Devnet) BuildOldCodeImage(ctx context.Context) error {
	if d.cfg.OldCodeSourceDir == "" {
		return fmt.Errorf("OldCodeSourceDir not set")
	}

	tag := oldCodeImageTag(d.cfg)

	// Write a Dockerfile tailored for the old code into the old source dir.
	dockerfile := filepath.Join(d.cfg.OldCodeSourceDir, "Dockerfile.devnet-old")
	content := `# syntax=docker/dockerfile:1
FROM golang:1.24.1 AS build
RUN apt update && apt install -y git python3
RUN useradd -m app
USER app
WORKDIR /home/app/app
RUN curl -sSf https://raw.githubusercontent.com/WasmEdge/WasmEdge/master/utils/install.sh | bash -s -- -v 0.13.4
COPY go.mod go.sum ./
RUN go mod download
COPY --chown=app:app . .
RUN . /home/app/.wasmedge/env && \
    go run github.com/99designs/gqlgen generate && \
    go build -buildvcs=false -ldflags "-X vsc-node/modules/announcements.GitCommit=$(git rev-parse HEAD)" -o magid vsc-node/cmd/vsc-node

FROM rockylinux:9.3-minimal
RUN microdnf install -y iptables && microdnf clean all
RUN useradd -m app
RUN mkdir -p /data/mapping-bot /data/devnet && chown app:app /data/mapping-bot /data/devnet
USER app
WORKDIR /home/app/app
COPY --from=build /home/app/app/magid .
COPY --from=build /home/app/.wasmedge /home/app/.wasmedge
ENV LD_LIBRARY_PATH=/home/app/.wasmedge/lib
ENV PATH=/home/app/.wasmedge/bin:$PATH
RUN printf '#!/bin/sh\n. /home/app/.wasmedge/env\nexec "$@"\n' > /home/app/app/entrypoint.sh && \
    chmod +x /home/app/app/entrypoint.sh
ENTRYPOINT ["/home/app/app/entrypoint.sh"]
`
	if err := os.WriteFile(dockerfile, []byte(content), 0o644); err != nil {
		return fmt.Errorf("writing old-code Dockerfile: %w", err)
	}
	defer os.Remove(dockerfile)

	log.Printf("[devnet] building old-code image %s from %s", tag, d.cfg.OldCodeSourceDir)
	cmd := exec.CommandContext(ctx, "docker", "build",
		"-t", tag,
		"-f", dockerfile,
		d.cfg.OldCodeSourceDir,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// Logs returns the docker compose logs for a service.
func (d *Devnet) Logs(ctx context.Context, service string) (string, error) {
	return d.composeOutput(ctx, "logs", "--no-color", service)
}

// compose runs a docker compose command with output streamed to stdout/stderr.
func (d *Devnet) compose(ctx context.Context, args ...string) error {
	fullArgs := append(
		[]string{"compose", "-f", d.composeFile, "-f", d.overrideFile, "--env-file", d.envFile, "-p", d.projectName},
		args...,
	)
	cmd := exec.CommandContext(ctx, "docker", fullArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	log.Printf("[devnet] $ docker %s", strings.Join(fullArgs, " "))
	return cmd.Run()
}

// composeOutput runs a docker compose command and captures its output.
func (d *Devnet) composeOutput(ctx context.Context, args ...string) (string, error) {
	fullArgs := append(
		[]string{"compose", "-f", d.composeFile, "-f", d.overrideFile, "--env-file", d.envFile, "-p", d.projectName},
		args...,
	)
	cmd := exec.CommandContext(ctx, "docker", fullArgs...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// waitForService polls a docker compose service until its healthcheck
// reports "healthy" or the timeout expires.
func (d *Devnet) waitForService(ctx context.Context, service string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			logs, _ := d.Logs(ctx, service)
			return fmt.Errorf("service %q not healthy after %v\nlast logs:\n%s",
				service, timeout, truncateLogs(logs, 50))
		}

		out, err := d.composeOutput(ctx, "ps", "--format", "{{.Health}}", service)
		if err == nil && strings.TrimSpace(out) == "healthy" {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// truncateLogs returns the last n lines of s.
func truncateLogs(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}
