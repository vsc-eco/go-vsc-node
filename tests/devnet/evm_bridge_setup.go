//go:build evm_devnet

// Shared L1-bridge devnet setup for W1/W2/W3 — the validated W-MOCK foundation
// (deploy short-timelock account-mapping + mock verifier, wire setVerifierContract
// through the short timelock, RC-bootstrap the owner) extracted into a reusable
// env, plus helpers to plant anvil headers and run owner admin ops.
package devnet

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

// --- shared devnet: ONE 5-node devnet for the whole evm_devnet run, shared by
// all (parallel) waves. Bring-up (~12 min) happens once; each wave deploys its
// own state-isolated contracts on top. The runner docker-cleans after the
// process exits (no per-test Stop). ---
var (
	sharedDevnetOnce sync.Once
	sharedDevnet     *Devnet
	sharedDevnetErr  error
	sharedDeployMu   sync.Mutex   // DeployContract stops a node (badger lock) — serialize
	anvilPortSeq     int32 = 38544 // each wave gets a unique anvil port via nextAnvilPort
	gqlNodeSeq       int32        // round-robins GQL reads across nodes 2-5 (spread parallel load)
)

func ensureSharedDevnet() (*Devnet, error) {
	sharedDevnetOnce.Do(func() {
		ctx := context.Background() // long-lived: outlives individual test timeouts
		cfg := evmBridgeConfig()
		d, err := New(cfg)
		if err != nil {
			sharedDevnetErr = err
			return
		}
		if err := d.Start(ctx); err != nil {
			sharedDevnetErr = err
			return
		}
		// Generous RC bootstrap — ALL parallel waves' owner ops (deploys, admin)
		// draw on magi.test1, so give it a big HBD/RC pool.
		if _, err := d.Deposit(ctx, 1, "80000.000", "hbd"); err != nil {
			sharedDevnetErr = fmt.Errorf("RC bootstrap deposit: %w", err)
			return
		}
		if err := pollUntil(ctx, 8*time.Minute, func() bool {
			bal, e := d.GetAccountBalance(ctx, 2, "hive:magi.test1")
			return e == nil && bal != nil && bal.Hbd >= 40_000_000
		}); err != nil {
			sharedDevnetErr = fmt.Errorf("RC bootstrap never credited magi.test1: %w", err)
			return
		}
		sharedDevnet = d
	})
	return sharedDevnet, sharedDevnetErr
}

// nextAnvilPort returns a unique anvil port so parallel waves don't collide.
func nextAnvilPort() string { return fmt.Sprintf("%d", atomic.AddInt32(&anvilPortSeq, 1)) }

// deployWithRetry wraps DeployContract to tolerate the transient
// "failed to request storage proof: context deadline exceeded" that the
// contract-deployer hits under I/O/CPU pressure on a constrained box (it
// surfaces as "could not parse contract ID from output"). DeployContract always
// restarts the stopped node even on failure, so the devnet is left retryable;
// we wait for it to settle, then retry. Without this, a single transient deploy
// timeout fails an entire wave whose contract logic is fine.
func deployWithRetry(ctx context.Context, d *Devnet, opts ContractDeployOpts, attempts int) (string, error) {
	var lastErr error
	for i := 0; i < attempts; i++ {
		id, err := d.DeployContract(ctx, opts)
		if err == nil {
			return id, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(25 * time.Second): // let the restarted node reconnect
		}
	}
	return "", fmt.Errorf("deploy %q failed after %d attempts: %w", opts.Name, attempts, lastErr)
}

type evmBridgeEnv struct {
	t         *testing.T
	ctx       context.Context
	d         *Devnet
	amID      string
	mockID    string
	ownerNode int
	gqlNode   int
	ownerAcct string
	propID    uint64 // next proposal id (monotonic)
}

// evmBridgeConfig builds the standard 5-node fast-config used by the L1 waves.
func evmBridgeConfig() *Config {
	cfg := regressionConfig()
	cfg.Nodes = 5
	cfg.GenesisNode = 5
	cfg.SysConfigOverrides.ConsensusParams.ElectionInterval = 20
	cfg.SysConfigOverrides.ConsensusParams.BondInclusionActivationHeight = 0
	cfg.GQLBasePort = 38080
	cfg.P2PBasePort = 31720
	cfg.MongoPort = 38057
	cfg.HivePort = 38091
	cfg.DronePort = 39000
	cfg.BitcoindRPCPort = 38543
	cfg.DashdRPCPort = 39898
	return cfg
}

// setupEvmBridgeDevnet stands up the full wired foundation and returns the env.
func setupEvmBridgeDevnet(t *testing.T, ctx context.Context) *evmBridgeEnv {
	requireDocker(t)
	d, err := ensureSharedDevnet()
	if err != nil {
		t.Fatalf("shared devnet: %v", err)
	}
	// Spread GQL reads/deploy-GQL across nodes 2-5 so parallel waves don't all
	// hammer node 2 (which caused "server closed connection" drops under -parallel 5).
	gqlNode := 2 + int(atomic.AddInt32(&gqlNodeSeq, 1)-1)%4
	e := &evmBridgeEnv{t: t, ctx: ctx, d: d, ownerNode: 1, gqlNode: gqlNode, propID: 1}
	e.ownerAcct = "hive:magi.test1"

	// Deploy this wave's own state-isolated contracts on the shared devnet.
	// Serialize: DeployContract stops the deployer node (badger lock).
	sharedDeployMu.Lock()
	amID, derr := deployWithRetry(ctx, d, ContractDeployOpts{WasmPath: evmBridgeWasmDevnet, Name: "evm-bridge", DeployerNode: e.ownerNode, GQLNode: e.gqlNode}, 3)
	var mockID string
	if derr == nil {
		mockID, derr = deployWithRetry(ctx, d, ContractDeployOpts{WasmPath: mockVerifierWasm, Name: "mock-zk-verifier", DeployerNode: e.ownerNode, GQLNode: e.gqlNode}, 3)
	}
	sharedDeployMu.Unlock()
	if derr != nil {
		t.Fatalf("deploy: %v", derr)
	}
	e.amID = amID
	e.mockID = mockID
	t.Logf("contracts: account-mapping=%s mock=%s", amID, mockID)

	if _, err := d.CallContract(ctx, e.ownerNode, amID, "initContract", `{"is_testnet":true}`); err != nil {
		t.Fatalf("init account-mapping: %v", err)
	}
	if _, err := d.CallContract(ctx, e.ownerNode, mockID, "init", `{}`); err != nil {
		t.Fatalf("init mock: %v", err)
	}

	// Wire setVerifierContract through the short timelock.
	if err := e.proposeExecute("setVerifierContract", fmt.Sprintf(`{"contract_id":"%s"}`, mockID)); err != nil {
		t.Fatalf("wire verifier: %v", err)
	}
	if err := pollUntil(ctx, 60*time.Second, func() bool {
		m, _ := d.GetStateByKeys(ctx, e.gqlNode, amID, []string{"zkverifier"})
		return m != nil && fmt.Sprintf("%v", m["zkverifier"]) == mockID
	}); err != nil {
		t.Fatalf("setVerifierContract did not take effect")
	}
	t.Logf("WIRED: account-mapping -> mock verifier")
	return e
}

// proposeExecute runs an owner admin action through the short (devnet) timelock:
// propose -> wait > TimelockLong(5) -> execute. payloadJSON is the action's
// inner JSON (hex-encoded into payload_hex).
func (e *evmBridgeEnv) proposeExecute(action, payloadJSON string) error {
	d := e.d
	payloadHex := hex.EncodeToString([]byte(payloadJSON))
	if _, err := d.CallContract(e.ctx, e.ownerNode, e.amID, "propose",
		fmt.Sprintf(`{"action":"%s","payload_hex":"%s"}`, action, payloadHex)); err != nil {
		return fmt.Errorf("propose %s: %w", action, err)
	}
	id := e.propID
	e.propID++
	lb, _, err := d.LocalNodeInfo(e.ctx, e.gqlNode)
	if err != nil {
		return err
	}
	if err := d.WaitForBlockProcessing(e.ctx, e.gqlNode, lb+12, 6*time.Minute); err != nil {
		return fmt.Errorf("await timelock: %w", err)
	}
	if _, err := d.CallContract(e.ctx, e.ownerNode, e.amID, "execute", fmt.Sprintf(`{"proposal_id":%d}`, id)); err != nil {
		return fmt.Errorf("execute %s: %w", action, err)
	}
	return nil
}

// setVault sets the bridge vault address (short-timelock admin action).
func (e *evmBridgeEnv) setVault(addr string) error {
	if err := e.proposeExecute("setVault", fmt.Sprintf(`"%s"`, addr)); err != nil {
		return err
	}
	return pollUntil(e.ctx, 60*time.Second, func() bool {
		m, _ := e.d.GetStateByKeys(e.ctx, e.gqlNode, e.amID, []string{"vault"})
		return m != nil && m["vault"] != nil && fmt.Sprintf("%v", m["vault"]) != ""
	})
}

// callOnce broadcasts a vsc.call EXACTLY once (no retry), for non-idempotent
// actions like `map`/`confirmSpend` where CallContract's 3x broadcast retry can
// double-process (vsc.call has no L2 nonce dedup). Signs as witness `node`.
func (e *evmBridgeEnv) callOnce(node int, contractID, action, payload string) (string, error) {
	callTx := map[string]interface{}{
		"net_id":      e.d.netId(),
		"contract_id": contractID,
		"action":      action,
		"payload":     payload,
		"rc_limit":    5000000,
		"intents":     []interface{}{},
	}
	witness := fmt.Sprintf("%s%d", e.d.cfg.WitnessPrefix, node)
	return e.d.BroadcastCustomJSON("vsc.call", []string{witness}, callTx, e.d.cfg.InitminerWIF)
}

// fundDID transfers HBD from the owner witness to an arbitrary L2 DID (e.g. an
// evm did:pkh) so it has RC. The unmapETH caller is the depositor's evm DID,
// which (not being "hive:") gets no free RC — it needs a real HBD balance.
func (e *evmBridgeEnv) fundDID(did, hbdAmount string) error {
	witness := fmt.Sprintf("%s%d", e.d.cfg.WitnessPrefix, e.ownerNode)
	payload := map[string]interface{}{
		"from":   e.ownerAcct, // hive:magi.testN
		"to":     did,         // did:pkh:... (vsc.transfer accepts did:/hive: recipients)
		"amount": hbdAmount,
		"asset":  "hbd",
		"net_id": e.d.netId(),
	}
	if _, err := e.d.BroadcastCustomJSON("vsc.transfer", []string{witness}, payload, e.d.cfg.InitminerWIF); err != nil {
		return err
	}
	return pollUntil(e.ctx, 2*time.Minute, func() bool {
		bal, err := e.d.GetAccountBalance(e.ctx, e.gqlNode, did)
		return err == nil && bal != nil && bal.Hbd > 0
	})
}

// depositETH performs a full ETH deposit (L1 anvil -> mock-planted header -> map)
// and returns the credited L2 DID. The relayer (ownerAcct) must be registered.
// amountWei is the deposit value; the credited gwei = amountWei/1e9 minus 1% gas tax.
func (e *evmBridgeEnv) depositETH(anvil *Anvil, depKey *ecdsa.PrivateKey, vaultAddr ethcommon.Address, amountWei *big.Int) (string, error) {
	ctx := e.ctx
	depAddr := ethCrypto.PubkeyToAddress(depKey.PublicKey)
	if err := anvil.SetBalance(ctx, depAddr, new(big.Int).Add(amountWei, big.NewInt(1e18))); err != nil {
		return "", err
	}
	txHash, blockM, err := anvil.SendETH(ctx, depKey, vaultAddr, amountWei)
	if err != nil {
		return "", fmt.Errorf("deposit send: %w", err)
	}
	_ = anvil.Mine(ctx, 6)
	if err := pollUntil(ctx, 60*time.Second, func() bool { f, e2 := anvil.Finalized(ctx); return e2 == nil && f >= blockM }); err != nil {
		return "", fmt.Errorf("deposit block %d not finalized", blockM)
	}
	hdr, err := anvil.Header(ctx, blockM)
	if err != nil {
		return "", err
	}
	if err := e.plantHeader(hdr, 1); err != nil {
		return "", err
	}
	txIndex, err := anvil.FindTxIndex(ctx, blockM, txHash)
	if err != nil {
		return "", err
	}
	rawHex, proofHex, err := anvil.TxTrieProof(ctx, blockM, txIndex)
	if err != nil {
		return "", err
	}
	payload := fmt.Sprintf(`{"tx_data":{"block_height":%d,"tx_index":%d,"raw_hex":"%s","merkle_proof_hex":"%s","deposit_type":"eth"},"instructions":[]}`,
		blockM, txIndex, rawHex, proofHex)
	if _, err := e.callOnce(e.ownerNode, e.amID, "map", payload); err != nil {
		return "", fmt.Errorf("map: %w", err)
	}
	did := "did:pkh:eip155:1:" + strings.ToLower(depAddr.Hex())
	balKey := "a-" + did + "-eth"
	if err := pollUntil(ctx, 3*time.Minute, func() bool {
		m, _ := e.d.GetStateByKeys(ctx, e.gqlNode, e.amID, []string{balKey})
		return m != nil && m[balKey] != nil && fmt.Sprintf("%v", m[balKey]) != "" && fmt.Sprintf("%v", m[balKey]) != "0"
	}); err != nil {
		return "", fmt.Errorf("deposit not credited to %s", did)
	}
	return did, nil
}

// getTssPubKey returns the contract's "primary" TSS public key (empty until keygen completes).
func (e *evmBridgeEnv) getTssPubKey() string {
	const q = `query($id:String!){ getTssKey(keyId:$id){ public_key } }`
	var out struct {
		GetTssKey *struct {
			PublicKey string `json:"public_key"`
		} `json:"getTssKey"`
	}
	if err := e.d.gqlQuery(e.ctx, e.gqlNode, q, map[string]any{"id": e.amID + "-primary"}, &out); err != nil {
		return ""
	}
	if out.GetTssKey == nil {
		return ""
	}
	return out.GetTssKey.PublicKey
}

// createTssKey schedules the contract's primary TSS keygen (createKey, short
// timelock) and waits for it to complete. unmapETH's requireTssKey gates on it.
func (e *evmBridgeEnv) createTssKey(timeout time.Duration) error {
	if err := e.proposeExecute("createKey", "{}"); err != nil {
		return err
	}
	return pollUntil(e.ctx, timeout, func() bool { return e.getTssPubKey() != "" })
}

// plantHeader stores an anvil block header into the mock verifier (real roots).
func (e *evmBridgeEnv) plantHeader(h *AnvilHeader, chainID uint64) error {
	plant := fmt.Sprintf(`{"block_number":%d,"state_root":"%s","tx_root":"%s","receipts_root":"%s","base_fee":%d,"gas_limit":%d,"timestamp":%d,"chain_id":%d}`,
		h.Number, h.StateRoot, h.TxRoot, h.ReceiptsRoot, h.BaseFee, h.GasLimit, h.Timestamp, chainID)
	if _, err := e.d.CallContract(e.ctx, e.ownerNode, e.mockID, "devSetHeader", plant); err != nil {
		return fmt.Errorf("devSetHeader %d: %w", h.Number, err)
	}
	return pollUntil(e.ctx, 60*time.Second, func() bool {
		got, _ := e.d.getStateHex(e.ctx, e.gqlNode, e.mockID, fmt.Sprintf("b-%d", h.Number))
		return got != ""
	})
}
