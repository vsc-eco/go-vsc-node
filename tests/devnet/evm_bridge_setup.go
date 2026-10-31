//go:build evm_devnet

// Shared L1-bridge devnet setup for W1/W2/W3 — the validated W-MOCK foundation
// (deploy short-timelock account-mapping + mock verifier, wire setVerifierContract
// through the short timelock, RC-bootstrap the owner) extracted into a reusable
// env, plus helpers to plant anvil headers and run owner admin ops.
package devnet

import (
	"context"
	"crypto/ecdsa"
	"encoding/asn1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/vsc-eco/hivego"
)

// --- shared devnet: ONE 5-node devnet for the whole evm_devnet run, shared by
// all (parallel) waves. Bring-up (~12 min) happens once; each wave deploys its
// own state-isolated contracts on top. The runner docker-cleans after the
// process exits (no per-test Stop). ---
var (
	sharedDevnetOnce sync.Once
	sharedDevnet     *Devnet
	sharedDevnetErr  error
	sharedDeployMu   sync.Mutex         // DeployContract stops a node (badger lock) — serialize
	anvilPortSeq     int32      = 28544 // each wave gets a unique anvil port via nextAnvilPort; below the ephemeral range (hostports.go)
	gqlNodeSeq       int32              // round-robins GQL reads across nodes 2-5 (spread parallel load)
)

func ensureSharedDevnet() (*Devnet, error) {
	sharedDevnetOnce.Do(func() {
		ctx := context.Background() // long-lived: outlives individual test timeouts
		cfg := evmBridgeConfig()
		// EVM_CONSENSUS_FLOOR=13 pins the version floor to 0.13 from epoch 1,
		// as the C1 custody devnet does, and the waves wait until the chain
		// runs it, so the rules gated on it (TSS-KG-1, the EVM key custody)
		// are in force when the bridge keys are created, as on a network that
		// upgrades before it deploys a bridge. Unset, the chain stays on the
		// default floor and the waves start at once.
		floor := os.Getenv("EVM_CONSENSUS_FLOOR")
		if floor != "" {
			n, err := strconv.ParseUint(floor, 10, 64)
			if err != nil {
				sharedDevnetErr = fmt.Errorf("EVM_CONSENSUS_FLOOR %q: %w", floor, err)
				return
			}
			cp := cfg.SysConfigOverrides.ConsensusParams
			cp.ConsensusVersionFloorMajor, cp.ConsensusVersionFloorConsensus, cp.ConsensusVersionFloorEpoch = 0, n, 1
		}
		d, err := New(cfg)
		if err != nil {
			sharedDevnetErr = err
			return
		}
		if err := d.Start(ctx); err != nil {
			sharedDevnetErr = err
			return
		}
		afterRun = append(afterRun, func() { _ = d.Stop() }) // torn down once every wave is done
		if err := bootstrapEvmOwner(ctx, d); err != nil {
			sharedDevnetErr = err
			return
		}
		if floor != "" {
			if err := waitForConsensus(ctx, d, "0."+floor, 20*time.Minute); err != nil {
				sharedDevnetErr = err
				return
			}
		}
		sharedDevnet = d
	})
	return sharedDevnet, sharedDevnetErr
}

// waitForConsensus waits until node 2 reports a chain-active version on the
// want line ("0.12" matches 0.12.x).
func waitForConsensus(ctx context.Context, d *Devnet, want string, within time.Duration) error {
	var ver string
	if err := pollUntil(ctx, within, func() bool {
		var out struct {
			LocalNodeInfo struct {
				V string `json:"consensus_version_display"`
			} `json:"localNodeInfo"`
		}
		if err := d.gqlQuery(ctx, 2, `{ localNodeInfo { consensus_version_display } }`, nil, &out); err != nil {
			return false
		}
		ver = out.LocalNodeInfo.V
		return strings.HasPrefix(ver, want+".")
	}); err != nil {
		return fmt.Errorf("chain-active version %q never reached %s.x: %w", ver, want, err)
	}
	log.Printf("[devnet] chain runs %s; starting the waves", ver)
	return nil
}

// bootstrapEvmOwner funds the owner (magi.test1) on devnet d: L2 HBD for RC and
// L1 TBD for contract deploy fees.
func bootstrapEvmOwner(ctx context.Context, d *Devnet) error {
	// Generous RC bootstrap — ALL parallel waves' owner ops (deploys, admin)
	// draw on magi.test1, so give it a big HBD/RC pool.
	// The witnesses' own L1 TBD (funding.go) is far below this, so initminer
	// pays it into magi.test1's L2 ledger through the gateway deposit path.
	if err := fundL2FromInitminer(d, "magi.test1", "150000.000"); err != nil {
		return fmt.Errorf("RC bootstrap deposit: %w", err)
	}
	// Every contract deploy also pays a 10 TBD fee from magi.test1's L1
	// balance; the waves deploy ~16 contracts, more than its 100 TBD.
	if err := fundL1FromInitminer(d, "magi.test1", "2000.000"); err != nil {
		return fmt.Errorf("L1 deploy-fee funding: %w", err)
	}
	if err := pollUntil(ctx, 8*time.Minute, func() bool {
		bal, e := d.GetAccountBalance(ctx, 2, "hive:magi.test1")
		return e == nil && bal != nil && bal.Hbd >= 40_000_000
	}); err != nil {
		return fmt.Errorf("RC bootstrap never credited magi.test1: %w", err)
	}
	return nil
}

// nextAnvilPort returns a unique anvil port so parallel waves don't collide.
// A port something already listens on is skipped (an anvil orphaned by an
// earlier run answers there with a chain hours long).
func nextAnvilPort() string {
	for i := 0; i < 50; i++ {
		p := fmt.Sprintf("%d", atomic.AddInt32(&anvilPortSeq, 1))
		if portFree(p) {
			return p
		}
		log.Printf("[devnet] anvil port %s is in use (left over from an earlier run?); trying the next", p)
	}
	return fmt.Sprintf("%d", atomic.AddInt32(&anvilPortSeq, 1))
}

// portFree reports whether nothing listens on 127.0.0.1:port.
func portFree(port string) bool {
	l, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}

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
		case <-devnetAfter(25 * time.Second): // let the restarted node reconnect
		}
	}
	return "", fmt.Errorf("deploy %q failed after %d attempts: %w", opts.Name, attempts, lastErr)
}

// evmChainID is the L1 chain id of every EVM wave: anvil, the bridge's
// initContract chain_id, planted headers and the did:pkh the bridge credits.
// Sepolia, not 1, so a path that still assumes mainnet's id fails here.
const evmChainID uint64 = 11155111

// evmL2Wait bounds every wait for an L2 state change (a call landing, a credit,
// a queued withdrawal). Each contract deploy restarts magi-1, and a producer
// still catching up composes slots its peers refuse (CID mismatch), so with
// several waves the L2 can go minutes without a block: 3.5 min in phase C,
// about one block a minute in phase D (2026-10-21). Waits on anvil itself stay
// short.
const evmL2Wait = 6 * time.Minute

var evmInitPayload = fmt.Sprintf(`{"chain_id":%d,"is_testnet":true}`, evmChainID)

// evmDID is the L2 identity the bridge credits for an L1 address: the node's
// did:pkh:eip155:1: namespace with the EIP-55 address, on every L1 chain
// (account-mapping C6/E21). It is also the identity the user signs as.
func evmDID(addr ethcommon.Address) string {
	return "did:pkh:eip155:1:" + addr.Hex()
}

// evmArtifact returns the path in env var name, failing the test when it is
// unset or missing. tests/devnet/suite/evm-artifacts.sh builds every EVM
// artifact from its source repo and prints the exports.
func evmArtifact(t *testing.T, name string) string {
	t.Helper()
	p := os.Getenv(name)
	if p == "" {
		t.Fatalf("%s is not set; build the EVM devnet artifacts with tests/devnet/suite/evm-artifacts.sh", name)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("%s=%s: %v", name, p, err)
	}
	return p
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
	// botAttests leaves the vault check signature (E23) to the bot under test
	// instead of tssVault.
	botAttests bool
}

// evmBridgeConfig builds the standard 5-node fast-config used by the L1 waves.
func evmBridgeConfig() *Config {
	cfg := regressionConfig()
	// The bot polls and anvil mines on wall clock, so the L2 chain keeps real
	// cadence: a fast-blocks run would let L2 deadlines lapse before the bot acts.
	cfg.RealCadence = true
	cfg.Nodes = 5
	cfg.GenesisNode = 5
	cfg.SysConfigOverrides.ConsensusParams.ElectionInterval = 20
	cfg.SysConfigOverrides.ConsensusParams.BondInclusionActivationHeight = 0
	return cfg
}

// setupEvmBridgeDevnet stands up the full wired foundation and returns the env.
func setupEvmBridgeDevnet(t *testing.T, ctx context.Context) *evmBridgeEnv {
	requireDocker(t)
	d, err := ensureSharedDevnet()
	if err != nil {
		t.Fatalf("shared devnet: %v", err)
	}
	return setupEvmBridgeOn(t, ctx, d)
}

// setupEvmBridgeOn deploys this test's bridge + mock verifier on devnet d and
// wires them (setupEvmBridgeDevnet does it on the shared devnet).
func setupEvmBridgeOn(t *testing.T, ctx context.Context, d *Devnet) *evmBridgeEnv {
	// Spread GQL reads/deploy-GQL across nodes 2-5 so parallel waves don't all
	// hammer node 2 (which caused "server closed connection" drops under -parallel 5).
	gqlNode := 2 + int(atomic.AddInt32(&gqlNodeSeq, 1)-1)%4
	e := &evmBridgeEnv{t: t, ctx: ctx, d: d, ownerNode: 1, gqlNode: gqlNode, propID: 1}
	e.ownerAcct = "hive:magi.test1"

	// Deploy this wave's own state-isolated contracts on the shared devnet.
	// Serialize: DeployContract stops the deployer node (badger lock).
	sharedDeployMu.Lock()
	amID, derr := deployWithRetry(ctx, d, ContractDeployOpts{WasmPath: evmArtifact(t, "EVM_BRIDGE_DEVNET_WASM"), Name: "evm-bridge", DeployerNode: e.ownerNode, GQLNode: e.gqlNode}, 3)
	var mockID string
	if derr == nil {
		mockID, derr = deployWithRetry(ctx, d, ContractDeployOpts{WasmPath: evmArtifact(t, "EVM_MOCK_VERIFIER_WASM"), Name: "mock-zk-verifier", DeployerNode: e.ownerNode, GQLNode: e.gqlNode}, 3)
	}
	sharedDeployMu.Unlock()
	if derr != nil {
		t.Fatalf("deploy: %v", derr)
	}
	e.amID = amID
	e.mockID = mockID
	t.Logf("contracts: account-mapping=%s mock=%s", amID, mockID)

	if _, err := d.CallContract(ctx, e.ownerNode, amID, "initContract", evmInitPayload); err != nil {
		t.Fatalf("init account-mapping: %v", err)
	}
	if _, err := d.CallContract(ctx, e.ownerNode, mockID, "init", `{}`); err != nil {
		t.Fatalf("init mock: %v", err)
	}

	// Wire setVerifierContract through the short timelock.
	if err := e.proposeExecute("setVerifierContract", fmt.Sprintf(`{"contract_id":"%s"}`, mockID)); err != nil {
		t.Fatalf("wire verifier: %v", err)
	}
	if err := pollUntil(ctx, evmL2Wait, func() bool {
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
	if err := e.awaitTimelock(); err != nil {
		return err
	}
	if _, err := d.CallContract(e.ctx, e.ownerNode, e.amID, "execute", fmt.Sprintf(`{"proposal_id":%d}`, id)); err != nil {
		return fmt.Errorf("execute %s: %w", action, err)
	}
	return nil
}

// awaitTimelock waits out the short devnet admin timelock after a propose.
// The timelock runs on the Hive block height (env.BlockHeight), so it waits on
// the Hive head. A GQL node's own processed height can trail it: a node
// catching up passed lb+12 seconds after the propose, the execute landed
// inside the timelock and was refused (ZK wave, phase B 2026-10-21).
func (e *evmBridgeEnv) awaitTimelock() error { return awaitHiveTimelock(e.ctx, e.d, 6*time.Minute) }

// awaitHiveTimelock waits until the Hive head is 12 blocks past where it is
// now (the devnet admin timelock is 5).
func awaitHiveTimelock(ctx context.Context, d *Devnet, timeout time.Duration) error {
	head, err := hiveHead(d)
	if err != nil {
		return err
	}
	if err := pollUntil(ctx, timeout, func() bool { h, err := hiveHead(d); return err == nil && h >= head+12 }); err != nil {
		return fmt.Errorf("await timelock: %w", err)
	}
	return nil
}

// hiveHead is the devnet Hive chain's head block.
func hiveHead(d *Devnet) (uint64, error) {
	raw, err := hivego.NewHiveRpc([]string{d.DroneEndpoint()}).GetDynamicGlobalProps()
	if err != nil {
		return 0, err
	}
	var p struct {
		HeadBlockNumber uint64 `json:"head_block_number"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return 0, err
	}
	return p.HeadBlockNumber, nil
}

// tssVault runs the contract's primary keygen and returns the vault: the
// key's L1 address, which the bridge derives itself (account-mapping A18) and
// treats as the vault once the key's check signature is attested (E23; done
// here unless botAttests).
func (e *evmBridgeEnv) tssVault(timeout time.Duration) (ethcommon.Address, error) {
	if err := e.createTssKey(timeout); err != nil {
		return ethcommon.Address{}, fmt.Errorf("TSS keygen: %w", err)
	}
	vault, ok := tssKeyAddress(e.getTssPubKey())
	if !ok {
		return ethcommon.Address{}, fmt.Errorf("TSS primary public key %q is not a secp256k1 key", e.getTssPubKey())
	}
	if !e.botAttests {
		if err := e.attestTssVault(vault, timeout); err != nil {
			return ethcommon.Address{}, fmt.Errorf("vault check signature (E23): %w", err)
		}
	}
	return vault, nil
}

// tssSign returns the bridge's queued tx signed by its TSS primary key: it
// waits for the network to sign the tx's sighash (getTssRequests), then
// attaches that signature with S made low and the recovery id that recovers to
// vault, as the bot does.
func (e *evmBridgeEnv) tssSign(unsignedHex string, vault ethcommon.Address, timeout time.Duration) (*ethtypes.Transaction, error) {
	inner, err := DecodeUnsigned1559(unsignedHex)
	if err != nil {
		return nil, err
	}
	signer := ethtypes.LatestSignerForChainID(inner.ChainID)
	unsigned := ethtypes.NewTx(inner)
	msg := hex.EncodeToString(signer.Hash(unsigned).Bytes())
	sig, err := e.tssLowS(msg, timeout)
	if err != nil {
		return nil, err
	}
	for v := byte(0); v < 2; v++ {
		sig[64] = v
		tx, err := unsigned.WithSignature(signer, sig)
		if err != nil {
			continue
		}
		if from, err := ethtypes.Sender(signer, tx); err == nil && from == vault {
			return tx, nil
		}
	}
	return nil, fmt.Errorf("TSS signature for %s does not recover to the vault %s", msg, vault.Hex())
}

// tssLowS waits for the network to sign msg (hex) with the bridge's primary
// key (getTssRequests) and returns r || s || 0 with S made low.
func (e *evmBridgeEnv) tssLowS(msg string, timeout time.Duration) ([]byte, error) {
	var der string
	if err := pollUntil(e.ctx, timeout, func() bool {
		reqs, err := e.d.GetTssRequests(e.ctx, e.gqlNode, e.amID+"-primary")
		for _, r := range reqs {
			if err == nil && strings.EqualFold(strings.TrimPrefix(r.Msg, "0x"), msg) && r.Status == "complete" && r.Sig != "" {
				der = r.Sig
				return true
			}
		}
		return false
	}); err != nil {
		return nil, fmt.Errorf("TSS signature for %s not complete: %w", msg, err)
	}
	derBytes, err := hex.DecodeString(strings.TrimPrefix(der, "0x"))
	if err != nil {
		return nil, err
	}
	var rs struct{ R, S *big.Int }
	if _, err := asn1.Unmarshal(derBytes, &rs); err != nil {
		return nil, fmt.Errorf("TSS signature is not DER: %w", err)
	}
	n := ethCrypto.S256().Params().N
	if rs.S.Cmp(new(big.Int).Rsh(n, 1)) > 0 {
		rs.S.Sub(n, rs.S)
	}
	sig := make([]byte, 65)
	rs.R.FillBytes(sig[:32])
	rs.S.FillBytes(sig[32:64])
	return sig, nil
}

// vaultCheckDigest is account-mapping's mapping.VaultCheckDigest (E23): the
// personal_sign digest the bridge's key signs before its address is the vault.
func vaultCheckDigest(contractID string, chainID uint64, pubHex string) []byte {
	msg := "magi evm vault check-signature v1:" + contractID + ":" + strconv.FormatUint(chainID, 10) + ":" + pubHex
	return ethCrypto.Keccak256([]byte("\x19Ethereum Signed Message:\n" + strconv.Itoa(len(msg)) + msg))
}

// attestTssVault puts the vault live (E23): the bridge asks its TSS key to
// sign the vault check message, and the network's signature is attested back,
// as the bot does on its own (cmd/evm-mapping-bot driveVaultCheck).
func (e *evmBridgeEnv) attestTssVault(vault ethcommon.Address, timeout time.Duration) error {
	pub := strings.ToLower(strings.TrimPrefix(e.getTssPubKey(), "0x"))
	if e.stateStr("vaultok") == pub {
		return nil
	}
	tx, err := e.callOnce(e.ownerNode, e.amID, "requestVaultCheck", `{}`)
	if err != nil {
		return fmt.Errorf("requestVaultCheck: %w", err)
	}
	if ok, msg := e.outResult(e.ctx, tx); !ok {
		return fmt.Errorf("requestVaultCheck (tx %s) failed: %s", tx, msg)
	}
	chainID, _ := strconv.ParseUint(e.stateStr("chainid"), 10, 64)
	if chainID == 0 {
		chainID = 1
	}
	digest := vaultCheckDigest(e.amID, chainID, pub)
	sig, err := e.tssLowS(hex.EncodeToString(digest), timeout)
	if err != nil {
		return err
	}
	for v := byte(0); v < 2; v++ {
		sig[64] = v
		if p, err := ethCrypto.SigToPub(digest, sig); err == nil && ethCrypto.PubkeyToAddress(*p) == vault {
			break
		}
	}
	tx, err = e.callOnce(e.ownerNode, e.amID, "attestVault", fmt.Sprintf(`{"signature":"%s"}`, hex.EncodeToString(sig)))
	if err != nil {
		return fmt.Errorf("attestVault: %w", err)
	}
	if ok, msg := e.outResult(e.ctx, tx); !ok {
		return fmt.Errorf("attestVault (tx %s) failed: %s", tx, msg)
	}
	return pollUntil(e.ctx, evmL2Wait, func() bool { return e.stateStr("vaultok") == pub })
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
	return pollUntil(e.ctx, evmL2Wait, func() bool {
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
	if err := e.plantHeader(hdr, evmChainID); err != nil {
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
	// FAILDEP-1: an ETH map carries the receipt proof so the contract can check
	// the tx succeeded (a reverted transfer moved nothing).
	_, rcptProofHex, err := anvil.ReceiptTrieProof(ctx, blockM, txIndex)
	if err != nil {
		return "", err
	}
	rcptProofB64, err := b64Proof(rcptProofHex)
	if err != nil {
		return "", err
	}
	payload := fmt.Sprintf(`{"tx_data":{"block_height":%d,"tx_index":%d,"raw_hex":"%s","merkle_proof_hex":"%s","receipt_proof":"%s","deposit_type":"eth"},"instructions":[]}`,
		blockM, txIndex, rawHex, proofHex, rcptProofB64)
	if _, err := e.callOnce(e.ownerNode, e.amID, "map", payload); err != nil {
		return "", fmt.Errorf("map: %w", err)
	}
	did := evmDID(depAddr)
	balKey := "a-" + did + "-eth"
	if err := pollUntil(ctx, evmL2Wait, func() bool {
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
	// 5 minutes, not 1: each contract deploy restarts magi-1, and with several
	// waves deploying, a producer that is still catching up composes slots its
	// peers refuse (CID mismatch), so the L2 can go a few minutes without a block
	// (3.5 min in phase C, 2026-10-21). The plant itself is one call.
	return pollUntil(e.ctx, 5*time.Minute, func() bool {
		got, _ := e.d.getStateHex(e.ctx, e.gqlNode, e.mockID, fmt.Sprintf("b-%d", h.Number))
		return got != ""
	})
}

// plantFinalized mines until anvil finalizes block n, then plants its header
// in the mock verifier: the job the ZK prover does on a live network.
func (e *evmBridgeEnv) plantFinalized(a *Anvil, n uint64) error {
	_ = a.Mine(e.ctx, 6)
	if err := pollUntil(e.ctx, 60*time.Second, func() bool { f, err := a.Finalized(e.ctx); return err == nil && f >= n }); err != nil {
		return fmt.Errorf("block %d not finalized", n)
	}
	h, err := a.Header(e.ctx, n)
	if err != nil {
		return err
	}
	return e.plantHeader(h, evmChainID)
}

// stateStr reads one bridge state key, "" when absent.
func (e *evmBridgeEnv) stateStr(key string) string {
	// A failed read is retried: returned as "", it reads as an absent key (a
	// zero balance), and phase C5 took one for a moved balance.
	var m map[string]any
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if m, err = e.d.GetStateByKeys(e.ctx, e.gqlNode, e.amID, []string{key}); err == nil || e.ctx.Err() != nil {
			break
		}
		devnetSleep(2 * time.Second)
	}
	if err != nil {
		e.t.Logf("stateStr %s: read failed: %v", key, err)
	}
	if m == nil || m[key] == nil {
		return ""
	}
	if s := fmt.Sprintf("%v", m[key]); s != "<nil>" {
		return s
	}
	return ""
}

// fundL2FromInitminer credits amount HBD to account's L2 ledger through the
// gateway deposit path (memo &to=account), paid by initminer, the devnet's
// TBD reserve.
func fundL2FromInitminer(d *Devnet, account, amount string) error {
	client := hivego.NewHiveRpc([]string{d.DroneEndpoint()})
	client.ChainID = devnetChainID
	wif := d.cfg.InitminerWIF
	op := hivego.TransferOperation{From: "initminer", To: "vsc.gateway", Amount: amount + " TBD", Memo: "&to=" + account}
	_, err := broadcastRetryingStale(client, []hivego.HiveOperation{op}, &wif, "evm RC bootstrap")
	return err
}

// fundL1FromInitminer sends amount TBD to account's Hive (L1) balance from
// initminer.
func fundL1FromInitminer(d *Devnet, account, amount string) error {
	client := hivego.NewHiveRpc([]string{d.DroneEndpoint()})
	client.ChainID = devnetChainID
	wif := d.cfg.InitminerWIF
	op := hivego.TransferOperation{From: "initminer", To: account, Amount: amount + " TBD", Memo: "evm deploy fees"}
	_, err := broadcastRetryingStale(client, []hivego.HiveOperation{op}, &wif, "evm deploy-fee funding")
	return err
}
