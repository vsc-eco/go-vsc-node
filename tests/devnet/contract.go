package devnet

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ContractDeployOpts holds options for deploying a WASM contract.
type ContractDeployOpts struct {
	// WasmPath is the host path to the compiled WASM bytecode file.
	WasmPath string
	// Name is the contract name.
	Name string
	// Description is an optional contract description.
	Description string
	// DeployerNode is which magi node's identity to use (1-indexed, default 1).
	DeployerNode int
	// GQLNode is which magi node to request storage proof from (1-indexed, default 1).
	GQLNode int
}

// DeployContract deploys a WASM contract to the running devnet.
// It uses the contract-deployer Docker service with the specified
// node's identity for signing the Hive transaction.
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

	wasmDir := filepath.Dir(wasmPath)
	wasmFile := filepath.Base(wasmPath)

	log.Printf("[devnet] deploying contract %q from %s (node %d)...",
		opts.Name, wasmPath, opts.DeployerNode)

	deployCmd := []string{
		"./contract-deployer",
		"-network=devnet",
		fmt.Sprintf("-data-dir=/data/devnet/data-%d", opts.DeployerNode),
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

// TestContractPath returns the absolute path to the built-in test
// WASM contract at modules/e2e/artifacts/contract_test.wasm.
func TestContractPath() string {
	return filepath.Join(findSourceRoot(), "modules", "e2e", "artifacts", "contract_test.wasm")
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
