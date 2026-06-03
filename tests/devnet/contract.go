package devnet

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// ContractDeployOpts holds options for deploying a WASM contract.
type ContractDeployOpts struct {
	// WasmPath is the host path to the compiled WASM bytecode file.
	WasmPath string
	// Name is the contract name.
	Name string
	// Description is an optional contract description.
	Description string
	// DeployerNode picks which magi node's identityConfig.json
	// donates the HiveUsername + HiveActiveKey for broadcasting the
	// L1 deploy tx + paying the deploy fee. Default 1. The
	// contract.owner field on the resulting contract resolves to
	// this node's Hive account. Note: the libp2p identity is ALWAYS
	// fresh (separate from any running magi node) so the deployer
	// joins the mesh without conflicting with the donor's libp2p
	// connections — the field name pre-dates the dedicated-deployer
	// refactor but the semantic "the magi node we borrow identity
	// from" is unchanged.
	DeployerNode int
	// GQLNode is which magi node to request storage proof from
	// (1-indexed, default 1). The deployer talks to this node over
	// HTTP for the election lookup; libp2p discovery handles the
	// signature-collection peers via DHT.
	GQLNode int
}

// deployerSetupOnce guards setupDeployerIdentity() so concurrent
// DeployContract callers don't race the init step.
var deployerSetupOnce sync.Once

// DeployContract deploys a WASM contract to the running devnet.
//
// The deployer runs in its own dedicated container with a FRESH
// libp2p identity (separate from any magi node) so no magi node has
// to be stopped to free a badger lock. The deployer borrows a magi
// node's Hive credentials (HiveUsername + HiveActiveKey + HiveURIs)
// so it can broadcast the L1 deploy tx + pay the per-deploy fee, but
// joins the libp2p mesh as a separate peer.
//
// Why this matters: the prior design stopped magi-1, ran the deployer
// reusing magi-1's data-dir + identity, then restarted magi-1. The
// other magi nodes still held stale connections to magi-1's
// pre-stop libp2p identity, and DHT discovery of the "reincarnated"
// peer took longer than the deployer's 15s RequestProof timeout —
// causing intermittent "subscription cancelled" failures with no
// contract id, especially under fast-fail circumstances. Tracked
// under "TestOracleDashChainRelay devnet flake investigation" in
// the dash_is_login_audit_loop memory note.
func (d *Devnet) DeployContract(ctx context.Context, opts ContractDeployOpts) (string, error) {
	if !d.started {
		return "", fmt.Errorf("devnet not started")
	}
	if opts.WasmPath == "" {
		return "", fmt.Errorf("wasm path is required")
	}
	if opts.Name == "" {
		return "", fmt.Errorf("contract name is required")
	}
	if opts.DeployerNode == 0 {
		opts.DeployerNode = 1
	}
	if opts.GQLNode == 0 {
		opts.GQLNode = 1
	}

	wasmPath, err := filepath.Abs(opts.WasmPath)
	if err != nil {
		return "", fmt.Errorf("resolving wasm path: %w", err)
	}
	if _, err := os.Stat(wasmPath); err != nil {
		return "", fmt.Errorf("wasm file not found: %w", err)
	}

	// Lazy one-shot setup of the deployer's identity. After this
	// runs, the deployer data-dir at <devnetDir>/deployer has:
	//   - identityConfig.json with magi-N's Hive creds but a fresh
	//     libp2p priv key + fresh BLS seed
	//   - hiveConfig.json with magi-N's HiveURIs (in-docker HAF
	//     endpoint)
	//   - p2pConfig.json with defaults (Bootnodes=[], peers via
	//     DHT)
	var setupErr error
	deployerSetupOnce.Do(func() {
		setupErr = d.setupDeployerIdentity(ctx, opts.DeployerNode)
	})
	if setupErr != nil {
		return "", fmt.Errorf("setting up deployer identity: %w", setupErr)
	}

	wasmDir := filepath.Dir(wasmPath)
	wasmFile := filepath.Base(wasmPath)

	log.Printf("[devnet] deploying contract %q (dedicated deployer, GQL via magi-%d)...",
		opts.Name, opts.GQLNode)

	deployCmd := []string{
		"./contract-deployer",
		"-network=devnet",
		"-data-dir=/data/devnet/deployer",
		fmt.Sprintf("-wasmPath=/wasm/%s", wasmFile),
		fmt.Sprintf("-name=%s", opts.Name),
		fmt.Sprintf("-gqlUrl=http://magi-%d:8080/api/v1/graphql", opts.GQLNode),
	}
	if opts.Description != "" {
		deployCmd = append(deployCmd, fmt.Sprintf("-description=%s", opts.Description))
	}

	args := []string{
		"run", "--rm",
		"-v", fmt.Sprintf("%s:/wasm", wasmDir),
		"contract-deployer",
	}
	args = append(args, deployCmd...)

	out, err := d.composeOutput(ctx, args...)
	if err != nil {
		return "", fmt.Errorf("contract deployment failed: %w\noutput: %s", err, out)
	}

	log.Printf("[devnet] contract-deployer output:\n%s", out)

	contractId := parseContractId(out)
	if contractId == "" {
		return "", fmt.Errorf("could not parse contract ID from output:\n%s", out)
	}

	log.Printf("[devnet] contract deployed: %s", contractId)
	return contractId, nil
}

// identityConfigFile mirrors modules/common/config.go:identityConfig.
// Kept private to this package since the harness only needs the
// subset of fields that drives the deployer-identity wiring.
type identityConfigFile struct {
	BlsPrivKeySeed string `json:"BlsPrivKeySeed"`
	HiveActiveKey  string `json:"HiveActiveKey"`
	HiveUsername   string `json:"HiveUsername"`
	Libp2pPrivKey  string `json:"Libp2pPrivKey"`
}

// setupDeployerIdentity creates a dedicated data-dir for the
// contract-deployer container at <devnetDir>/deployer, with:
//   - a fresh libp2p identity (so it doesn't conflict with any magi
//     node's existing libp2p mesh entry)
//   - magi-N's Hive credentials (so it can pay the deploy fee + the
//     contract.owner field resolves to a funded testnet account)
//   - magi-N's hiveConfig.json (so the in-docker HAF URI is reachable)
//
// Idempotent — re-running on an existing deployer dir is a no-op.
// Called once per Devnet lifetime via deployerSetupOnce.
func (d *Devnet) setupDeployerIdentity(ctx context.Context, donorNode int) error {
	deployerDir := filepath.Join(d.devnetDir, "deployer")
	if err := os.MkdirAll(deployerDir, 0o755); err != nil {
		return fmt.Errorf("mkdir deployer data-dir: %w", err)
	}

	// 1. Run `contract-deployer -init` to bootstrap fresh configs.
	//    This creates identityConfig.json + hiveConfig.json +
	//    p2pConfig.json with default values, but most importantly
	//    generates a fresh Libp2pPrivKey + BlsPrivKeySeed for us.
	log.Printf("[devnet] initialising dedicated deployer identity at %s...", deployerDir)
	initOut, err := d.composeOutput(ctx,
		"run", "--rm",
		"contract-deployer",
		"./contract-deployer",
		"-network=devnet",
		"-data-dir=/data/devnet/deployer",
		"-init",
	)
	if err != nil {
		return fmt.Errorf("contract-deployer -init: %w\noutput: %s", err, initOut)
	}

	// 2. Read the donor node's identityConfig.json. We borrow
	//    HiveUsername + HiveActiveKey from there.
	donorIdentityPath := filepath.Join(d.devnetDir,
		fmt.Sprintf("data-%d", donorNode), "identityConfig.json")
	donorBytes, err := os.ReadFile(donorIdentityPath)
	if err != nil {
		return fmt.Errorf("reading donor identityConfig at %s: %w", donorIdentityPath, err)
	}
	var donor identityConfigFile
	if err := json.Unmarshal(donorBytes, &donor); err != nil {
		return fmt.Errorf("parsing donor identityConfig: %w", err)
	}
	if donor.HiveUsername == "" || donor.HiveActiveKey == "" {
		return fmt.Errorf("donor magi-%d has empty Hive credentials — devnet bootstrap broken?", donorNode)
	}

	// 3. Read the freshly-initialised deployer identityConfig.json.
	deployerIdentityPath := filepath.Join(deployerDir, "identityConfig.json")
	deployerBytes, err := os.ReadFile(deployerIdentityPath)
	if err != nil {
		return fmt.Errorf("reading deployer identityConfig at %s: %w", deployerIdentityPath, err)
	}
	var deployer identityConfigFile
	if err := json.Unmarshal(deployerBytes, &deployer); err != nil {
		return fmt.Errorf("parsing deployer identityConfig: %w", err)
	}

	// 4. Patch deployer's Hive creds with donor's, KEEP deployer's
	//    fresh Libp2pPrivKey + BlsPrivKeySeed. The libp2p identity
	//    differing is THE point — no mesh-conflict with the donor's
	//    running magi node.
	deployer.HiveActiveKey = donor.HiveActiveKey
	deployer.HiveUsername = donor.HiveUsername

	merged, err := json.MarshalIndent(deployer, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling merged deployer identity: %w", err)
	}
	if err := os.WriteFile(deployerIdentityPath, merged, 0o600); err != nil {
		return fmt.Errorf("writing merged deployer identityConfig: %w", err)
	}

	// 5. Copy donor's hiveConfig.json verbatim — HiveURIs point at
	//    the in-docker HAF instance which the deployer needs.
	donorHivePath := filepath.Join(d.devnetDir,
		fmt.Sprintf("data-%d", donorNode), "hiveConfig.json")
	deployerHivePath := filepath.Join(deployerDir, "hiveConfig.json")
	hiveBytes, err := os.ReadFile(donorHivePath)
	if err != nil {
		return fmt.Errorf("reading donor hiveConfig at %s: %w", donorHivePath, err)
	}
	if err := os.WriteFile(deployerHivePath, hiveBytes, 0o600); err != nil {
		return fmt.Errorf("writing deployer hiveConfig: %w", err)
	}

	log.Printf("[devnet] deployer identity ready (donor=magi-%d, libp2p=fresh)", donorNode)
	return nil
}

// BuildCallTssContract builds the call-tss WASM contract using Docker
// TinyGo. Returns the path to the built .wasm file.
func BuildCallTssContract(ctx context.Context) (string, error) {
	contractDir := filepath.Join(findSourceRoot(), "tests", "devnet", "contracts", "call-tss")
	wasmPath := filepath.Join(contractDir, "bin", "build.wasm")

	log.Printf("[devnet] building call-tss contract...")
	cmd := exec.CommandContext(ctx, "make", "-C", contractDir, "build")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("building call-tss contract: %w", err)
	}

	if _, err := os.Stat(wasmPath); err != nil {
		return "", fmt.Errorf("built wasm not found at %s: %w", wasmPath, err)
	}

	log.Printf("[devnet] call-tss contract built: %s", wasmPath)
	return wasmPath, nil
}

// BuildBtcStubContract builds the btc-stub WASM contract used by oracle
// chain-relay devnet tests. The stub mimics the BTC mapping contract's
// `addBlocks` interface but skips all BTC validation, making it cheap to
// run inside a regtest devnet.
func BuildBtcStubContract(ctx context.Context) (string, error) {
	contractDir := filepath.Join(findSourceRoot(), "tests", "devnet", "contracts", "btc-stub")
	wasmPath := filepath.Join(contractDir, "bin", "build.wasm")

	log.Printf("[devnet] building btc-stub contract...")
	cmd := exec.CommandContext(ctx, "make", "-C", contractDir, "build")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("building btc-stub contract: %w", err)
	}

	if _, err := os.Stat(wasmPath); err != nil {
		return "", fmt.Errorf("built wasm not found at %s: %w", wasmPath, err)
	}

	log.Printf("[devnet] btc-stub contract built: %s", wasmPath)
	return wasmPath, nil
}

// TestContractPath returns the absolute path to the built-in test
// WASM contract at modules/e2e/artifacts/contract_test.wasm.
func TestContractPath() string {
	return filepath.Join(findSourceRoot(), "modules", "e2e", "artifacts", "contract_test.wasm")
}

// DashMappingContractPath returns the absolute path to the prebuilt
// dash-mapping-contract WASM used by the oracle Dash chain-relay devnet
// test. Unlike btc-stub, this is the REAL mapping contract that we expect
// to deploy on Magi testnet/mainnet.
//
// Resolution order:
//  1. DASH_MAPPING_WASM_PATH env var, if set
//  2. Sibling repo at <go-vsc-node>/../utxo-mapping/dash-mapping-contract/bin/testnet.wasm
//
// Returns an error (with a clear build hint) if the WASM cannot be
// located, so tests fail loudly instead of silently loading a stale or
// wrong-chain binary.
func DashMappingContractPath() (string, error) {
	candidates := make([]string, 0, 2)
	if p := os.Getenv("DASH_MAPPING_WASM_PATH"); p != "" {
		candidates = append(candidates, p)
	}
	candidates = append(candidates,
		filepath.Join(findSourceRoot(), "..", "utxo-mapping", "dash-mapping-contract", "bin", "testnet.wasm"),
	)

	for _, p := range candidates {
		abs, err := filepath.Abs(p)
		if err != nil {
			continue
		}
		if info, err := os.Stat(abs); err == nil && !info.IsDir() && info.Size() > 0 {
			return abs, nil
		}
	}

	return "", fmt.Errorf(
		"dash-mapping-contract WASM not found. Tried: %v\n"+
			"Build it first:\n"+
			"  cd <utxo-mapping>/dash-mapping-contract && USE_DOCKER=1 make testnet\n"+
			"Or set DASH_MAPPING_WASM_PATH to an absolute path.",
		candidates,
	)
}

func parseContractId(output string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "contract id:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "contract id:"))
		}
	}
	return ""
}
