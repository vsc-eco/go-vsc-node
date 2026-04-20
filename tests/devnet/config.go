package devnet

import (
	"fmt"
	"os"
	"path/filepath"

	systemconfig "vsc-node/modules/common/system-config"
)

// Config holds all configuration for a devnet test environment.
type Config struct {
	// Nodes is the number of VSC nodes to run (minimum 4).
	Nodes int
	// GQLBasePort is the host port for the first node's GraphQL API.
	// Subsequent nodes use GQLBasePort+1, +2, etc.
	GQLBasePort int
	// P2PBasePort is the internal P2P base port for magi nodes.
	// Each node gets P2PBasePort + n - 1.
	P2PBasePort int
	// MongoPort is the host port exposed for MongoDB.
	MongoPort int
	// HivePort is the host port exposed for the Hive RPC endpoint.
	HivePort int
	// DataDir is the root directory for all data. If empty, a temp dir is created.
	DataDir string
	// ProjectName is the docker compose project name. If empty, auto-generated.
	ProjectName string
	// WitnessPrefix is the prefix for witness account names.
	WitnessPrefix string
	// StakeAmount per witness (in TESTS, e.g. "2000.000").
	StakeAmount string
	// LogLevel for magi nodes (e.g. "error,tss=trace").
	LogLevel string
	// KeepRunning prevents teardown on Stop() for debugging.
	KeepRunning bool
	// HAFImage is the Docker image for the Hive testnet.
	HAFImage string
	// MongoImage is the Docker image for MongoDB.
	MongoImage string
	// SourceDir is the absolute path to the go-vsc-node repo root.
	SourceDir string
	// GenesisNode is which node runs the genesis election (1-indexed).
	GenesisNode int
	// InitminerWIF is the private key for the initminer account.
	InitminerWIF string
	// SysConfigOverrides, if non-nil, are written to a JSON file and
	// passed to each magid node via -sysconfig flag.
	SysConfigOverrides *systemconfig.SysConfigOverrides
	// SkipFunding skips the price feed + TBD transfer step. Set to true
	// for tests that don't need contract deployment funds (e.g., TSS tests).
	SkipFunding bool
	// OldCodeSourceDir is the path to an older version of the go-vsc-node
	// repo. If set, nodes listed in OldCodeNodes are built from this source
	// instead of SourceDir. Used for multi-version testing.
	OldCodeSourceDir string
	// OldCodeNodes lists which nodes (1-indexed) should run the old code
	// image built from OldCodeSourceDir.
	OldCodeNodes []int
	// HafahImage is the Docker image for the hafah SQL installer.
	HafahImage string
	// PostgRESTImage is the Docker image for hafah-postgrest (PostgREST).
	PostgRESTImage string
	// PgBouncerImage is the Docker image for pgbouncer connection pooler.
	PgBouncerImage string
	// DroneImage is the Docker image for the drone API router.
	DroneImage string
	// DronePort is the host port exposed for the drone API endpoint.
	DronePort int
	// BitcoindImage is the Docker image for the bitcoind regtest service
	// used by oracle chain-relay tests. Only started when EnableBitcoind
	// is true.
	BitcoindImage string
	// EnableBitcoind starts the bitcoind regtest service alongside the
	// devnet. Required for oracle chain-relay tests.
	EnableBitcoind bool
	// BitcoindRPCPort is the host port exposed for the archive bitcoind's
	// JSON-RPC. Used by tests that need direct RPC access from Go (e.g.
	// creating large transactions for pruning tests). Defaults to 18543.
	BitcoindRPCPort int
	// DashdImage is the Docker image for the dashd regtest service used by
	// oracle Dash chain-relay tests. Only started when EnableDashd is true.
	DashdImage string
	// EnableDashd starts the dashd regtest service alongside the devnet.
	// Required for oracle Dash chain-relay tests.
	EnableDashd bool
	// DashdRPCPort is the host port exposed for the dashd regtest JSON-RPC.
	// Defaults to 19898 (regtest is 19898 by default for Dash).
	DashdRPCPort int
}

// DefaultConfig returns a Config with sensible defaults for testing.
func DefaultConfig() *Config {
	return &Config{
		Nodes:           5,
		GQLBasePort:     18080,
		P2PBasePort:     11720, // offset from mainnet/testnet nodes on 10720+
		MongoPort:       18057,
		HivePort:        18091,
		WitnessPrefix:   "magi.test",
		StakeAmount:     "2000.000",
		LogLevel:        "error,tss=trace",
		HAFImage:        "registry.gitlab.syncad.com/hive/haf/testnet",
		MongoImage:      "mongo:8.0.17",
		SourceDir:       findSourceRoot(),
		GenesisNode:     5,
		InitminerWIF:    "5JNHfZYKGaomSFvd4NUdQ9qMcEAC43kujbfjueTHpVapX1Kzq2n",
		HafahImage:      "registry.gitlab.syncad.com/hive/hafah",
		PostgRESTImage:  "registry.gitlab.syncad.com/hive/haf_api_node/postgrest",
		PgBouncerImage:  "registry.gitlab.syncad.com/hive/haf_api_node/pgbouncer",
		DroneImage:      "registry.gitlab.syncad.com/hive/drone",
		DronePort:       19000,
		BitcoindImage:   "bitcoin/bitcoin:29.3",
		BitcoindRPCPort: 18543,
		DashdImage:      "dashpay/dashd:23.1.2",
		DashdRPCPort:    19898,
	}
}

// findSourceRoot locates the go-vsc-node repository root by walking
// up from CWD looking for go.mod.
func findSourceRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "."
		}
		dir = parent
	}
}

// Validate checks the config for errors.
func (c *Config) Validate() error {
	if c.Nodes < 4 {
		return fmt.Errorf("minimum 4 nodes required, got %d", c.Nodes)
	}
	if c.GenesisNode < 1 || c.GenesisNode > c.Nodes {
		return fmt.Errorf("genesis node must be 1-%d, got %d", c.Nodes, c.GenesisNode)
	}
	if c.SourceDir == "" {
		return fmt.Errorf("source directory not set")
	}
	if _, err := os.Stat(filepath.Join(c.SourceDir, "go.mod")); err != nil {
		return fmt.Errorf("source directory %q doesn't contain go.mod: %w", c.SourceDir, err)
	}
	return nil
}
