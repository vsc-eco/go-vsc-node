package devnet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"time"
)

// ContractUpdateOpts holds options for queueing a contract code update on the
// running devnet (drives the contract-deployer with -contractId + -wasmPath).
type ContractUpdateOpts struct {
	ContractId   string
	WasmPath     string
	Name         string
	DeployerNode int
	GQLNode      int
}

// ContractCancelOpts holds options for cancelling a pending contract update.
type ContractCancelOpts struct {
	ContractId   string
	DeployerNode int
	GQLNode      int
}

// runDeployer stops the deployer node (to release its badger lock), runs the
// contract-deployer container with the given args (optionally mounting a wasm
// dir at /wasm), then restarts the node. Mirrors DeployContract's lifecycle.
func (d *Devnet) runDeployer(ctx context.Context, deployerNode int, wasmDir string, deployArgs []string) (string, error) {
	if !d.started {
		return "", fmt.Errorf("devnet not started")
	}
	nodeName := fmt.Sprintf("magi-%d", deployerNode)

	if err := d.compose(ctx, "stop", nodeName); err != nil {
		return "", fmt.Errorf("stopping %s: %w", nodeName, err)
	}

	args := []string{"run", "--rm"}
	if wasmDir != "" {
		args = append(args, "-v", fmt.Sprintf("%s:/wasm", wasmDir))
	}
	args = append(args, "contract-deployer")
	args = append(args, deployArgs...)

	out, err := d.composeOutput(ctx, args...)

	if startErr := d.compose(ctx, "start", nodeName); startErr != nil {
		log.Printf("[devnet] warning: failed to restart %s: %v", nodeName, startErr)
	}
	time.Sleep(5 * time.Second)

	if err != nil {
		return "", fmt.Errorf("deployer run failed: %w\noutput: %s", err, out)
	}
	return out, nil
}

// resolveNodes fills in default/disjoint deployer and GQL nodes (the GQL node
// must differ from the deployer node, which is stopped during the run).
func (d *Devnet) resolveNodes(deployerNode, gqlNode int) (int, int) {
	if deployerNode == 0 {
		deployerNode = 1
	}
	if gqlNode == 0 {
		gqlNode = 1
	}
	if gqlNode == deployerNode {
		for i := 1; i <= d.cfg.Nodes; i++ {
			if i != deployerNode {
				gqlNode = i
				break
			}
		}
	}
	return deployerNode, gqlNode
}

// UpdateContract queues a code update for an existing contract. On a network with
// a timelock (devnet: 30 blocks) the new code does not become active immediately.
func (d *Devnet) UpdateContract(ctx context.Context, opts ContractUpdateOpts) error {
	if opts.ContractId == "" {
		return fmt.Errorf("contract id is required")
	}
	if opts.WasmPath == "" {
		return fmt.Errorf("wasm path is required")
	}
	deployerNode, gqlNode := d.resolveNodes(opts.DeployerNode, opts.GQLNode)

	wasmPath, err := filepath.Abs(opts.WasmPath)
	if err != nil {
		return fmt.Errorf("resolving wasm path: %w", err)
	}
	wasmDir := filepath.Dir(wasmPath)
	wasmFile := filepath.Base(wasmPath)
	name := opts.Name
	if name == "" {
		name = "timelock-test"
	}

	deployArgs := []string{
		"./contract-deployer",
		"-network=devnet",
		fmt.Sprintf("-data-dir=/data/devnet/data-%d", deployerNode),
		fmt.Sprintf("-wasmPath=/wasm/%s", wasmFile),
		fmt.Sprintf("-contractId=%s", opts.ContractId),
		fmt.Sprintf("-name=%s", name),
		fmt.Sprintf("-gqlUrl=http://magi-%d:8080/api/v1/graphql", gqlNode),
	}

	log.Printf("[devnet] queueing contract update for %s (stopping magi-%d)...", opts.ContractId, deployerNode)
	out, err := d.runDeployer(ctx, deployerNode, wasmDir, deployArgs)
	if err != nil {
		return err
	}
	log.Printf("[devnet] update-contract deployer output:\n%s", out)
	return nil
}

// CancelContractUpdate posts a vsc.cancel_contract_update for the contract,
// tombstoning any pending (timelocked) update.
func (d *Devnet) CancelContractUpdate(ctx context.Context, opts ContractCancelOpts) error {
	if opts.ContractId == "" {
		return fmt.Errorf("contract id is required")
	}
	deployerNode, gqlNode := d.resolveNodes(opts.DeployerNode, opts.GQLNode)

	deployArgs := []string{
		"./contract-deployer",
		"-network=devnet",
		fmt.Sprintf("-data-dir=/data/devnet/data-%d", deployerNode),
		"-cancel-update",
		fmt.Sprintf("-contractId=%s", opts.ContractId),
		fmt.Sprintf("-gqlUrl=http://magi-%d:8080/api/v1/graphql", gqlNode),
	}

	log.Printf("[devnet] cancelling pending update for %s (stopping magi-%d)...", opts.ContractId, deployerNode)
	out, err := d.runDeployer(ctx, deployerNode, "", deployArgs)
	if err != nil {
		return err
	}
	log.Printf("[devnet] cancel-update deployer output:\n%s", out)
	return nil
}

// ContractGQL is a contract record as returned by the node's GraphQL API.
type ContractGQL struct {
	Id               string
	Code             string
	Proposer         string
	CreationHeight   uint64
	ActivationHeight uint64
}

// graphQLContracts runs a contracts-returning query against a node's GraphQL API
// and decodes the named result field into []ContractGQL.
func (d *Devnet) graphQLContracts(ctx context.Context, node int, field, query string) ([]ContractGQL, error) {
	body, _ := json.Marshal(map[string]string{"query": query})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.GQLEndpoint(node), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var parsed struct {
		Data   map[string][]struct {
			Id               string  `json:"id"`
			Code             string  `json:"code"`
			Proposer         string  `json:"proposer"`
			CreationHeight   float64 `json:"creation_height"`
			ActivationHeight float64 `json:"activation_height"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("decoding graphql response: %w\nbody: %s", err, string(raw))
	}
	if len(parsed.Errors) > 0 {
		return nil, fmt.Errorf("graphql error: %s\nbody: %s", parsed.Errors[0].Message, string(raw))
	}
	rows := parsed.Data[field]
	out := make([]ContractGQL, 0, len(rows))
	for _, r := range rows {
		out = append(out, ContractGQL{
			Id:               r.Id,
			Code:             r.Code,
			Proposer:         r.Proposer,
			CreationHeight:   uint64(r.CreationHeight),
			ActivationHeight: uint64(r.ActivationHeight),
		})
	}
	return out, nil
}

// ActiveContract returns the version of a contract currently active at the chain
// head (via findContract), or nil if not found.
func (d *Devnet) ActiveContract(ctx context.Context, node int, id string) (*ContractGQL, error) {
	q := fmt.Sprintf(`{ findContract(filterOptions: {byId: %q}) { id code proposer creation_height activation_height } }`, id)
	rows, err := d.graphQLContracts(ctx, node, "findContract", q)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

// PendingUpdates returns queued (not-yet-active) updates for a contract via
// findPendingContractUpdates.
func (d *Devnet) PendingUpdates(ctx context.Context, node int, id string) ([]ContractGQL, error) {
	q := fmt.Sprintf(`{ findPendingContractUpdates(filterOptions: {byId: %q}) { id code proposer creation_height activation_height } }`, id)
	return d.graphQLContracts(ctx, node, "findPendingContractUpdates", q)
}

// WaitForActiveCode polls until the active version of a contract has the given
// code CID, or the timeout expires.
func (d *Devnet) WaitForActiveCode(ctx context.Context, node int, id, code string, timeout time.Duration) (*ContractGQL, error) {
	deadline := time.Now().Add(timeout)
	var last *ContractGQL
	for {
		c, err := d.ActiveContract(ctx, node, id)
		if err == nil && c != nil {
			last = c
			if c.Code == code {
				return c, nil
			}
		}
		if time.Now().After(deadline) {
			return last, fmt.Errorf("timeout: active code for %s is %v, want %s", id, last, code)
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// WaitForPendingUpdate polls until a pending update with the given code appears
// for a contract, returning it.
func (d *Devnet) WaitForPendingUpdate(ctx context.Context, node int, id, code string, timeout time.Duration) (*ContractGQL, error) {
	deadline := time.Now().Add(timeout)
	for {
		rows, err := d.PendingUpdates(ctx, node, id)
		if err == nil {
			for i := range rows {
				if rows[i].Code == code {
					return &rows[i], nil
				}
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timeout waiting for pending update %s with code %s (err=%v)", id, code, err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}
