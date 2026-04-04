package devnet

import (
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
func writeEnvFile(cfg *Config, hafDataDir, devnetDir, outputPath string) error {
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

	return os.WriteFile(outputPath, []byte(b.String()), 0o644)
}

// writeNodesOverride generates a docker-compose override file that
// defines the magi-1 … magi-N node services.  This is the only
// generated YAML — everything else lives in the static compose file.
func writeNodesOverride(cfg *Config, devnetDir, outputPath string) error {
	var b strings.Builder

	b.WriteString("services:\n")
	for i := 1; i <= cfg.Nodes; i++ {
		gqlPort := cfg.GQLBasePort + i - 1
		p2pPort := cfg.P2PBasePort + i - 1
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
    container_name: magi-%[1]d
    hostname: magi-%[1]d
    command: ["./magid", "-network", "devnet", "-data-dir", "/data/devnet/data-%[1]d", "-log-level", "%[3]s"]
    ports:
      - "%[4]d:8080"
      - "%[5]d:%[5]d"
      - "%[5]d:%[5]d/udp"
    volumes:
      - %[6]s:/data/devnet
`, i, cfg.SourceDir, cfg.LogLevel, gqlPort, p2pPort, devnetDir)
	}

	return os.WriteFile(outputPath, []byte(b.String()), 0o644)
}
