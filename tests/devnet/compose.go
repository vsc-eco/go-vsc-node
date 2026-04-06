package devnet

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// composeFilePath returns the path to the static docker-compose.yml
// (infrastructure services: HAF, MongoDB, setup tools).
func composeFilePath() string {
	return filepath.Join(findSourceRoot(), "tests", "devnet", "docker-compose.yml")
}

// writeEnvFile generates the .env file consumed by docker compose for
// variable substitution in both the base and override compose files.
func writeEnvFile(cfg *Config, hafDataDir, devnetDir, droneConfigPath, outputPath string) error {
	var b strings.Builder

	kv := func(k, v string) { fmt.Fprintf(&b, "%s=%s\n", k, v) }

	kv("SOURCE_DIR", cfg.SourceDir)
	kv("HAF_IMAGE", cfg.HAFImage)
	kv("MONGO_IMAGE", cfg.MongoImage)
	kv("HAF_DATA_DIR", hafDataDir)
	kv("DEVNET_DATA_DIR", devnetDir)
	kv("HIVE_PORT", fmt.Sprint(cfg.HivePort))
	kv("MONGO_PORT", fmt.Sprint(cfg.MongoPort))
	kv("DEVNET_NODES", fmt.Sprint(cfg.Nodes))
	kv("GENESIS_NODE", fmt.Sprint(cfg.GenesisNode))
	kv("LOG_LEVEL", cfg.LogLevel)
	kv("HAFAH_IMAGE", cfg.HafahImage)
	kv("POSTGREST_IMAGE", cfg.PostgRESTImage)
	kv("PGBOUNCER_IMAGE", cfg.PgBouncerImage)
	kv("DRONE_IMAGE", cfg.DroneImage)
	kv("DRONE_PORT", fmt.Sprint(cfg.DronePort))
	kv("DRONE_CONFIG_PATH", droneConfigPath)

	return os.WriteFile(outputPath, []byte(b.String()), 0o644)
}

// writeSysConfigOverrides writes the sysconfig override JSON file for
// each node into the devnet data directory. Returns the container path
// to the sysconfig file (same for all nodes since the file is identical).
func writeSysConfigOverrides(cfg *Config, devnetDir string) error {
	if cfg.SysConfigOverrides == nil {
		return nil
	}
	data, err := json.MarshalIndent(cfg.SysConfigOverrides, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling sysconfig overrides: %w", err)
	}
	path := filepath.Join(devnetDir, "sysconfig.json")
	return os.WriteFile(path, data, 0o644)
}

// writeNodesOverride generates a docker-compose override file that
// defines the magi-1 … magi-N node services. This is the only
// generated YAML — everything else lives in the static compose file.
//
// Each node gets NET_ADMIN capability for iptables-based network
// partition testing. If cfg.SysConfigOverrides is set, a sysconfig.json
// file is written and passed to each magid node via -sysconfig flag.
func writeNodesOverride(cfg *Config, devnetDir, outputPath string) error {
	var b strings.Builder

	b.WriteString("services:\n")
	for i := 1; i <= cfg.Nodes; i++ {
		gqlPort := cfg.GQLBasePort + i - 1
		p2pPort := cfg.P2PBasePort + i - 1

		// Build the magid command, optionally including sysconfig path.
		cmd := fmt.Sprintf(
			`"./magid", "-network", "devnet", "-data-dir", "/data/devnet/data-%d", "-log-level", "%s"`,
			i, cfg.LogLevel,
		)
		if cfg.SysConfigOverrides != nil {
			cmd += `, "-sysconfig", "/data/devnet/sysconfig.json"`
		}

		fmt.Fprintf(&b, `
  magi-%[1]d:
    build:
      context: %[2]s
      dockerfile: tests/devnet/Dockerfile.devnet
    depends_on:
      db:
        condition: service_healthy
    networks:
      - devnet
    cap_add:
      - NET_ADMIN
    container_name: magi-%[1]d
    hostname: magi-%[1]d
    command: [%[3]s]
    ports:
      - "%[4]d:8080"
      - "%[5]d:%[5]d"
      - "%[5]d:%[5]d/udp"
    volumes:
      - %[6]s:/data/devnet
`, i, cfg.SourceDir, cmd, gqlPort, p2pPort, devnetDir)
	}

	return os.WriteFile(outputPath, []byte(b.String()), 0o644)
}
