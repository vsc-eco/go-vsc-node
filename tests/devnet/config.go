package devnet

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"vsc-node/modules/common/params"
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
	// MongoMemory is the shared MongoDB container's memory limit (docker
	// notation, e.g. "4G"). Empty keeps the compose default of 1G; 16 nodes
	// creating their indexes at once exhaust 1G and the container drops them.
	MongoMemory string
	// StartBatch starts the magi nodes this many at a time, 20 s apart (0 = all
	// at once). Sixteen nodes creating their indexes together crashed MongoDB
	// 8.0.17 (signal 11 in createIndexes); batches keep that DDL apart.
	StartBatch int
	// SourceDir is the absolute path to the go-vsc-node repo root.
	SourceDir string
	// GenesisNode is which node runs the genesis election (1-indexed).
	GenesisNode int
	// InitminerWIF is the private key for the initminer account.
	InitminerWIF string
	// SysConfigOverrides, if non-nil, are written to a JSON file and
	// passed to each magid node via -sysconfig flag.
	SysConfigOverrides *systemconfig.SysConfigOverrides
	// NodeSysConfigOverrides gives single nodes their own overrides, layered
	// over SysConfigOverrides field by field (a set field replaces the shared
	// one whole), so one node can run with different parameters, e.g. a
	// shorter TSS timeout than its peers.
	NodeSysConfigOverrides map[int]*systemconfig.SysConfigOverrides
	// SkipFunding skips the price feed + TBD transfer step. Set to true
	// for tests that don't need contract deployment funds (e.g., TSS tests).
	SkipFunding bool
	// FastBlocks hands Hive block production to the harness once the magi nodes
	// are up (see pacemaker.go): each block is produced as soon as every node
	// reports itself quiescent, and never later than one Hive block interval
	// after the previous one, so the chain is never slower than real time.
	// Enables hived's debug_node_api. DEVNET_FASTBLOCKS=1 turns it on for every
	// devnet that allows it (applyFastModeEnv).
	FastBlocks bool
	// RealCadence keeps this devnet at the real 3s block cadence even under
	// DEVNET_FASTBLOCKS=1, for a test that depends on wall-clock behavior the
	// pacemaker cannot see.
	RealCadence bool
	// FastBlockMinInterval is the shortest gap between pacemaker blocks
	// (default 100ms); gossip convergence itself is gated (gossipDiverged).
	FastBlockMinInterval time.Duration
	// PreParamsFixtureDir, if set, holds pre-generated TSS pre-parameter sets
	// (*.json, see TestGeneratePreParamsFixture) that nodes use instead of
	// generating safe primes live (preparams_pool.go). Defaults to
	// $DEVNET_PREPARAMS_DIR.
	PreParamsFixtureDir string
	// LivePreParams keeps every node on live safe-prime generation even when
	// DEVNET_PREPARAMS_DIR is set, for a test whose subject is that generation
	// (a cold or exhausted pool, PreParamsTimeout under CPU contention): a
	// fixture set is claimed in milliseconds, so the stall such a test exists to
	// provoke would never happen and the test would pass without testing it.
	LivePreParams bool
	// OldCodeSourceDir is the path to an older version of the go-vsc-node
	// repo. If set, nodes listed in OldCodeNodes are built from this source
	// instead of SourceDir. Used for multi-version testing.
	OldCodeSourceDir string
	// OldCodeNodes lists which nodes (1-indexed) should run the old code
	// image built from OldCodeSourceDir.
	OldCodeNodes []int
	// OldCodeSysconfig passes -sysconfig to the OldCodeNodes too. Set it when
	// OldCodeSourceDir is a same-version variant of this tree (for example a
	// node patched to misbehave) that must run the same network parameters.
	OldCodeSysconfig bool
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
	// MagiEnv is a per-test env-var override map applied to every magi-N
	// container. Use for devnet-only knobs that don't deserve a SysConfig
	// field — e.g. VSC_GATEWAY_ROTATION_INTERVAL to drive FUZZ-1 paths
	// in seconds instead of an hour.
	MagiEnv map[string]string
	// OldCodeGoImage overrides the Go base image used by BuildOldCodeImage.
	// Defaults to "golang:1.25.10", which must stay >= the toolchain in the
	// OldCodeSourceDir's go.mod (currently `go 1.25.7`) or the build fails
	// under GOTOOLCHAIN=local. Override only to pin an older baseline for
	// multiversion gossip-resilience runs, and only when that source tree's
	// go.mod actually accepts it.
	OldCodeGoImage string
}

// devnetPortOffset shifts every HOST port binding by a fixed amount, so several
// devnets can run at once.
//
// The base ports are constants, so two concurrent devnets collide immediately on
// the first bind and one dies with "port is already allocated" — which made the
// vault campaign strictly serial at roughly half an hour per test. Container and
// network names already carry a random per-devnet id; host ports were the only
// thing shared. Set DEVNET_PORT_OFFSET to a distinct multiple of 1000 per lane.
//
// Defaults to 0, so a single devnet behaves exactly as before.
func devnetPortOffset() int {
	raw := os.Getenv("DEVNET_PORT_OFFSET")
	if raw == "" {
		return 0
	}
	off, err := strconv.Atoi(raw)
	if err != nil || off < 0 {
		return 0
	}
	return off
}

// DefaultConfig returns a Config with sensible defaults for testing.
func DefaultConfig() *Config {
	off := devnetPortOffset()
	return &Config{
		Nodes:           5,
		GQLBasePort:     18080 + off,
		P2PBasePort:     11720 + off, // offset from mainnet/testnet nodes on 10720+
		MongoPort:       18057 + off,
		HivePort:        18091 + off,
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
		DronePort:       19000 + off,
		BitcoindImage:   "bitcoin/bitcoin:29.3",
		BitcoindRPCPort: 18543 + off,
		DashdImage:      "dashpay/dashd:23.1.2",
		DashdRPCPort:    19898 + off,
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
	if c.FastBlocks && len(c.OldCodeNodes) > 0 {
		// Old-code images have no /devnet/quiescence probe, so the pacemaker could
		// not gate on them.
		return fmt.Errorf("FastBlocks cannot be combined with OldCodeNodes")
	}
	return nil
}

// applyFastModeEnv applies the suite-wide fast-mode switches. DEVNET_FASTBLOCKS=1
// turns FastBlocks on unless the test set RealCadence or runs old-code nodes
// (their images have no quiescence probe); it logs which. DEVNET_PREPARAMS_DIR
// supplies the pre-params fixture to every devnet, at either cadence, so a fast
// run and a real one differ only in block production, except to a test that sets
// LivePreParams. A test's own settings win.
func applyFastModeEnv(cfg *Config) {
	if cfg.LivePreParams {
		cfg.PreParamsFixtureDir = ""
	} else if cfg.PreParamsFixtureDir == "" {
		cfg.PreParamsFixtureDir = os.Getenv("DEVNET_PREPARAMS_DIR")
	}
	if os.Getenv("DEVNET_FASTBLOCKS") != "1" || cfg.FastBlocks {
		return
	}
	switch {
	case cfg.RealCadence:
		log.Printf("[devnet] DEVNET_FASTBLOCKS=1, but this test sets RealCadence: real 3s blocks")
	case len(cfg.OldCodeNodes) > 0:
		log.Printf("[devnet] DEVNET_FASTBLOCKS=1, but old-code nodes have no quiescence probe: real 3s blocks")
	default:
		cfg.FastBlocks = true
	}
}

// applyFloorPinFromEnv pins the network's consensus version floor from the
// environment, for running a whole suite at a version line without editing each
// test: DEVNET_FLOOR_CONSENSUS=9 pins 0.9.0 from epoch 1 (DEVNET_FLOOR_MAJOR
// defaults to 0). It overrides a floor the test set itself, so it must only be
// used for suites whose tests are meant to hold at that line. Unset: no change.
func applyFloorPinFromEnv(cfg *Config) {
	v := os.Getenv("DEVNET_FLOOR_CONSENSUS")
	if v == "" {
		return
	}
	consensus, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return
	}
	major := uint64(0)
	if m := os.Getenv("DEVNET_FLOOR_MAJOR"); m != "" {
		if parsed, err := strconv.ParseUint(m, 10, 64); err == nil {
			major = parsed
		}
	}
	if cfg.SysConfigOverrides == nil {
		cfg.SysConfigOverrides = &systemconfig.SysConfigOverrides{}
	}
	if cfg.SysConfigOverrides.ConsensusParams == nil {
		cfg.SysConfigOverrides.ConsensusParams = &params.ConsensusParams{}
	}
	// FloorEpoch must be non-zero: 0 means "no floor".
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorMajor = major
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorConsensus = consensus
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorEpoch = 1
}

// waitForFloorPin holds Start until the network's election carries the version
// pinned by DEVNET_FLOOR_CONSENSUS. The pin applies from epoch 1, so without
// this wait a short test runs most of its subject in epoch 0 at version 0.0.
// No-op when the variable is unset.
func (d *Devnet) waitForFloorPin(ctx context.Context) error {
	v := os.Getenv("DEVNET_FLOOR_CONSENSUS")
	if v == "" {
		return nil
	}
	want, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return nil
	}
	log.Printf("[devnet] waiting for the election to carry consensus version 0.%d (DEVNET_FLOOR_CONSENSUS)", want)
	for {
		var out struct {
			ElectionByBlockHeight struct {
				Epoch           uint64 `json:"epoch"`
				ProtocolVersion uint64 `json:"protocol_version"`
			} `json:"electionByBlockHeight"`
		}
		err := d.gqlQuery(ctx, 1, `{ electionByBlockHeight { epoch protocol_version } }`, nil, &out)
		if err == nil && out.ElectionByBlockHeight.ProtocolVersion >= want {
			log.Printf("[devnet] consensus version 0.%d active from epoch %d",
				out.ElectionByBlockHeight.ProtocolVersion, out.ElectionByBlockHeight.Epoch)
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for consensus version 0.%d: %w", want, ctx.Err())
		case <-devnetAfter(10 * time.Second):
		}
	}
}

// vfTestBudget scales a test's own context budget for the environment it runs in.
//
// These per-test budgets were tuned against a box running ONE devnet. The campaign runs
// three at once, so everything takes longer, and a test that overruns its own context does
// not fail on its subject - it fails on whatever call happened to be in flight, with an
// EMPTY status that reads as a refusal. That is exactly how Stage-5's VL-GP-10b failed:
// 38m0s budget, 2299.86s elapsed, and the final retireVault came back "".
//
// DEVNET_TIMEOUT_SCALE (default 1, so a solo run is byte-identical) multiplies the budget.
// The result is capped at 60 minutes so the context still expires BEFORE `go test -timeout
// 65m` kills the process: a context deadline unwinds cleanly and reports its cases, while a
// go-test timeout panics the whole binary and loses them. Budgets already above the cap are
// left alone - they never bind anyway.
// vfDevnetBudgetCap is the ceiling on any single test's OWN context, and the one
// number `go test -timeout` must stay above.
//
// A test whose context outlives its runner cannot produce a verdict. When the
// runner's timeout fires first, Go panics with a goroutine dump instead of
// cancelling the context, so the test's own summary never runs: no PASS, no FAIL,
// no case list — just a stack trace the campaign has to score as NO-VERDICT. That
// is strictly worse than a failure, because a failure names its cause.
//
// This used to be a `const cap = 60m` with an early `if base >= cap { return base }`,
// which meant the cap applied to nobody who needed it: 26 of the 45 vault tests ask
// for 70-120m, took the bypass, and ran with a context that could never expire
// before a 65m runner. TestVaultF3CrashMidKeygen is what that looks like from the
// outside — `panic: test timed out after 1h5m0s` with its last four cases passing.
//
// Raise it and the runner's -timeout TOGETHER, never one alone.
func vfDevnetBudgetCap() time.Duration {
	if v := os.Getenv("DEVNET_BUDGET_CAP"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return 75 * time.Minute
}

func vfTestBudget(base time.Duration) time.Duration {
	scale := 1.0
	if v := os.Getenv("DEVNET_TIMEOUT_SCALE"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			scale = f
		}
	}
	cap := vfDevnetBudgetCap()
	d := time.Duration(float64(base) * scale)
	if d < base {
		d = base
	}
	if d > cap {
		// Loud, because a silently shortened budget looks like the subject failing
		// late rather than the instrument stopping early.
		log.Printf("[devnet] test budget %s (base %s x scale %.2f) truncated to the %s cap; "+
			"raise DEVNET_BUDGET_CAP and `go test -timeout` together if this test needs longer",
			d.Round(time.Minute), base, scale, cap)
		d = cap
	}
	return d
}
