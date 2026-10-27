//go:build evm_devnet && evm_realproof

// WREAL: the EVM bridge end to end on REAL data. L1 is Sepolia (public RPCs),
// the verifier is the real zk-header-verifier, and every header the bridge
// relies on arrives through a real Groth16 proof bought on the Succinct Prover
// Network by the sp1-helios-magi operator (magi_operator), plus the operator's
// header backfill. Nothing is planted.
//
// Core flow (a failure stops the test):
//
//	(0) TSS keygen gives the vault; the real bot attests it (E23) and takes it (A18).
//	(1) init the verifier from magi_genesis at the live finalized checkpoint, pin the ELF.
//	(2) Sepolia deposits from a funded test wallet: 0.04975 ETH to the gas
//	    reserve (calldata "to_reserve=1"), 0.025 ETH for the user (its 1% tax
//	    brings the reserve to exactly the 0.05 floor), 2 USDC (Circle's Sepolia
//	    FiatToken v2).
//	(3) T1: once they are finalized, one paid proof; the verifier accepts it
//	    (sp1_verify_groth16 under the pinned VK) and the operator backfills.
//	(4) the bot maps all three deposits from the proven headers.
//	(5a) the user withdraws 0.01 ETH; the TSS committee signs it with the
//	    heaviest node down, the bot broadcasts it to Sepolia, the recipient is
//	    paid. While it is pending, the user's USDC withdrawal is refused (RP-11).
//	(6a) T2: a second paid proof covers it (E5 on real data); a new bot, started
//	    from the first one's checkpoint, closes it with confirmSpend.
//	(5b)/(6b) the user withdraws 1 USDC; T3, a third paid proof, covers it and
//	    the bot closes it. A further ETH withdrawal is then refused by the
//	    reserve floor (RP-4).
//	(7) replay: a bot with a fresh checkpoint rescans everything; nothing is
//	    credited twice.
//
// Operations shaped like mainnet's (rollout notes, 10-18 weights):
//
//	M1  a stake-weighted committee shaped like mainnet's (30/27/20/13/10% for
//	    v4vapp 24.5, magi-team-node 22.7, tibfox 16.6, ...), the heaviest on node 4.
//	M2  the vault key reshares each epoch while deposits and withdrawals run;
//	    its address never changes.
//	M3  two honest bots overlap (as during a bot move), both on a public RPC;
//	    nothing is credited, sent or confirmed twice. RPC errors are counted.
//
// Failure states and dangerous surfaces (a failure is reported, the run goes on):
//
//	F1  T1's real proof with one byte flipped: refused by the Groth16 check.
//	F2  T1's real proof submitted again: refused (the head does not advance).
//	F3  T1's real proof with its public values tampered: refused by the Groth16 check.
//	F4  with a header gap open, a real Sepolia header with one field changed:
//	    refused; the same header unchanged: accepted.
//	F5  a relayer forges a deposit: another tx's real trie proof with a raw tx
//	    that pays the vault: refused; the real deposit replayed: refused.
//	F6  confirmSpend with another withdrawal's tx, a raised amount, or the wrong
//	    block: refused; the real one replayed after it closed: refused.
//	F7  the operator against a verifier whose vkey is not this ELF's: no paid proof.
//	F8  the operator killed mid-backfill and restarted with --max-proofs 0:
//	    the gap closes and no second proof is bought.
//	F9  the operator with an empty requester balance: no proof, no spend.
//	F10 the confirming bot lists a dead RPC first: it fails over, and does not
//	    send the vault's withdrawals again.
//	F11 a deposit claimed in a block above the proven height: refused.
//	F12 the heaviest node (node 4, ~30% of the weight, mainnet's v4vapp role)
//	    down while the TSS signs the withdrawals; the chain and TSS carry on; it rejoins.
//	F13 the operator handed a mainnet execution RPC: no paid proof.
//	F14 a bot handed a mainnet RPC (RP-5): observed.
//	F15 deposits as legacy (type 0) and access-list (type 1) txs: credited.
//	F16 a deposit forwarded by a contract (an internal tx, E18): observed.
//
// It spends real PROVE (three proofs, each capped by --max-proofs 1), so it
// needs its own build tag and never runs with the evm_devnet suite.
//
// Inputs (env):
//
//	WREAL_SEPOLIA_KEY_FILE    hex key of the funded Sepolia test wallet
//	WREAL_REQUESTER_KEY_FILE  hex key of the funded Prover Network requester
//	MAGI_OPERATOR_BIN, MAGI_GENESIS_BIN   sp1-helios-magi release binaries
//	ZK_TX_SIGNER_DIR          directory holding the zk-tx-signer the operator runs
//	WREAL_LOG_DIR             where the operator logs go
//	WREAL_SOURCE_DIR          optional: build the node image from this tree
//	WREAL_EXEC_RPC, WREAL_BEACON_RPC, WREAL_MAINNET_RPC   optional endpoints
//	plus the evm-artifacts.sh exports (EVM_BRIDGE_DEVNET_WASM, ZK_VERIFIER_WASM, EVM_BOT_BIN)
//
// Run: go test -tags 'evm_devnet evm_realproof' -run TestEVMBridge_WReal ./tests/devnet/ -timeout 420m -v
package devnet

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"vsc-node/lib/dids"

	ethereum "github.com/ethereum/go-ethereum"
	ethcommon "github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rlp"
	ethrpc "github.com/ethereum/go-ethereum/rpc"
)

const (
	wrealExecRPC    = "https://ethereum-sepolia-rpc.publicnode.com"
	wrealBeaconRPC  = "http://unstable.sepolia.beacon-api.nimbus.team"
	wrealMainnetRPC = "https://ethereum.publicnode.com"
	// Circle's USDC on Sepolia (FiatToken v2, the same code as mainnet USDC).
	wrealUSDC = "0x1c7D4B196Cb0C7B01d743Fbc6116a902379C7238"
	// sha256 of sp1-helios-magi elf/light_client at audit-review-7-med f614748.
	wrealElfHash = "989f58744e3c75062c74966a3a64a4feb17d505da966b0ae0b229678df68eeba"
)

// sepolia is a client for the live L1.
type sepolia struct {
	cli     *ethclient.Client
	rpc     *ethrpc.Client
	chainID *big.Int
}

func dialSepolia(t *testing.T, ctx context.Context, url string) *sepolia {
	t.Helper()
	rc, err := ethrpc.DialContext(ctx, url)
	if err != nil {
		t.Fatalf("dial Sepolia %s: %v", url, err)
	}
	cli := ethclient.NewClient(rc)
	id, err := cli.ChainID(ctx)
	if err != nil || id.Uint64() != evmChainID {
		t.Fatalf("Sepolia RPC %s: chain id %v (%v), want %d", url, id, err, evmChainID)
	}
	return &sepolia{cli: cli, rpc: rc, chainID: id}
}

// proofs returns the anvil harness's trie-proof helpers bound to Sepolia (they
// only use standard JSON-RPC).
func (s *sepolia) proofs(url string) *Anvil {
	return &Anvil{URL: url, ChainID: s.chainID, cli: s.cli, rpc: s.rpc}
}

// sendTx signs txdata (nonce and chain id filled in) with key and waits for
// its receipt. Sepolia's base fee is tiny, so 1 gwei tips under a 20 gwei cap
// always land.
func (s *sepolia) sendTx(ctx context.Context, key *ecdsa.PrivateKey, build func(nonce uint64) ethtypes.TxData) (*ethtypes.Receipt, error) {
	nonce, err := s.cli.PendingNonceAt(ctx, cryptoPubkeyAddr(key))
	if err != nil {
		return nil, fmt.Errorf("nonce: %w", err)
	}
	tx, err := ethtypes.SignTx(ethtypes.NewTx(build(nonce)), ethtypes.LatestSignerForChainID(s.chainID), key)
	if err != nil {
		return nil, fmt.Errorf("sign: %w", err)
	}
	if err := s.cli.SendTransaction(ctx, tx); err != nil {
		return nil, fmt.Errorf("send: %w", err)
	}
	var rc *ethtypes.Receipt
	if err := pollUntil(ctx, 5*time.Minute, func() bool {
		r, rerr := s.cli.TransactionReceipt(ctx, tx.Hash())
		rc = r
		return rerr == nil && r != nil
	}); err != nil {
		return nil, fmt.Errorf("no receipt for %s within 5 min", tx.Hash().Hex())
	}
	return rc, nil
}

func (s *sepolia) send(ctx context.Context, key *ecdsa.PrivateKey, to ethcommon.Address, value *big.Int, data []byte, gas uint64) (*ethtypes.Receipt, error) {
	return s.sendTx(ctx, key, func(n uint64) ethtypes.TxData {
		return &ethtypes.DynamicFeeTx{ChainID: s.chainID, Nonce: n, GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(20e9), Gas: gas, To: &to, Value: value, Data: data}
	})
}

func (s *sepolia) finalized(ctx context.Context) (uint64, error) {
	h, err := s.cli.HeaderByNumber(ctx, big.NewInt(int64(ethrpc.FinalizedBlockNumber)))
	if err != nil {
		return 0, err
	}
	return h.Number.Uint64(), nil
}

// waitFinalized waits for Sepolia to finalize block n (about 13 to 19 min of
// wall clock after it is mined: Sepolia is a live chain, not the devnet's).
func (s *sepolia) waitFinalized(ctx context.Context, n uint64, within time.Duration) error {
	deadline := time.Now().Add(within) // wall: Sepolia finality runs on its own wall clock
	for {
		if f, err := s.finalized(ctx); err == nil && f >= n {
			return nil
		}
		if time.Now().After(deadline) { // wall: as above
			return fmt.Errorf("Sepolia block %d not finalized within %s", n, within)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Second): // wall: as above
		}
	}
}

func (s *sepolia) usdcBalance(ctx context.Context, who ethcommon.Address) (*big.Int, error) {
	usdc := ethcommon.HexToAddress(wrealUSDC)
	out, err := s.cli.CallContract(ctx, ethereum.CallMsg{To: &usdc, Data: usdcCall("balanceOf(address)", who)}, nil)
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(out), nil
}

// vaultTx finds the vault's L1 tx at nonce, scanning forward from *next; the
// scan position carries over between polls so each block is read once.
func (s *sepolia) vaultTx(ctx context.Context, vault ethcommon.Address, nonce uint64, next *uint64) (*ethtypes.Transaction, *ethtypes.Receipt, bool) {
	sent, err := s.cli.NonceAt(ctx, vault, nil)
	if err != nil || sent <= nonce {
		return nil, nil, false // not mined yet: skip the block scan
	}
	latest, err := s.cli.BlockNumber(ctx)
	if err != nil {
		return nil, nil, false
	}
	signer := ethtypes.LatestSignerForChainID(s.chainID)
	for ; *next <= latest; *next++ {
		blk, err := s.cli.BlockByNumber(ctx, new(big.Int).SetUint64(*next))
		if err != nil {
			return nil, nil, false
		}
		for _, tx := range blk.Transactions() {
			if from, err := ethtypes.Sender(signer, tx); err == nil && from == vault && tx.Nonce() == nonce {
				rc, err := s.cli.TransactionReceipt(ctx, tx.Hash())
				if err != nil {
					return nil, nil, false
				}
				return tx, rc, true
			}
		}
	}
	return nil, nil, false
}

func readHexKey(t *testing.T, env string) (*ecdsa.PrivateKey, string) {
	t.Helper()
	path := os.Getenv(env)
	if path == "" {
		t.Fatalf("%s is not set", env)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v", env, err)
	}
	h := strings.TrimPrefix(strings.TrimSpace(string(raw)), "0x")
	k, err := ethCrypto.HexToECDSA(h)
	if err != nil {
		t.Fatalf("%s does not hold a secp256k1 hex key", env) // never print the key
	}
	return k, h
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// runOperator runs magi_operator until it exits, `within` passes, or stop
// matches a log line (then it is killed, as a crash would). Its env carries
// both keys, so its output goes only to logPath.
func runOperator(t *testing.T, ctx context.Context, logPath string, env, args []string, within time.Duration, stop func(string) bool) (out string, stopped bool, err error) {
	t.Helper()
	octx, cancel := context.WithTimeout(ctx, within) // wall: the Prover Network auction runs on wall clock
	defer cancel()
	cmd := exec.CommandContext(octx, evmArtifact(t, "MAGI_OPERATOR_BIN"), args...)
	cmd.Env = env
	cmd.SysProcAttr = dieWithTest()
	f, err := os.Create(logPath)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	cmd.Stdout, cmd.Stderr = f, f
	if err := cmd.Start(); err != nil {
		return "", false, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	seen := 0
	for {
		select {
		case err := <-done:
			b, _ := os.ReadFile(logPath)
			return string(b), stopped, err
		case <-time.After(2 * time.Second): // wall: as above
			if stop == nil || stopped {
				continue
			}
			b, _ := os.ReadFile(logPath)
			lines := strings.Split(string(b), "\n")
			for ; seen < len(lines)-1; seen++ {
				if stop(lines[seen]) {
					_ = cmd.Process.Kill()
					stopped = true
					break
				}
			}
		}
	}
}

// operatorLines returns the operator log lines that carry one of the markers.
func operatorLines(log string, markers ...string) []string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(log))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		for _, m := range markers {
			if strings.Contains(sc.Text(), m) {
				out = append(out, sc.Text())
				break
			}
		}
	}
	return out
}

// callPayload returns the contract-call payload carried by L2 tx txID.
func callPayload(ctx context.Context, d *Devnet, txID string) (string, error) {
	const q = `query($id:String!){findTransaction(filterOptions:{byId:$id}){ops{type data}}}`
	var out struct {
		FindTransaction []struct {
			Ops []struct {
				Data map[string]any `json:"data"`
			} `json:"ops"`
		} `json:"findTransaction"`
	}
	if err := d.gqlQuery(ctx, 2, q, map[string]any{"id": txID}, &out); err != nil {
		return "", err
	}
	for _, tx := range out.FindTransaction {
		for _, op := range tx.Ops {
			switch p := op.Data["payload"].(type) {
			case string:
				return p, nil
			case nil:
			default:
				b, _ := json.Marshal(p)
				return string(b), nil
			}
		}
	}
	return "", fmt.Errorf("no call payload in L2 tx %s", txID)
}

// flipHex changes one hex digit in the middle of s.
func flipHex(s string) string {
	b := []byte(s)
	i := len(b) / 2
	if b[i] == '0' {
		b[i] = '1'
	} else {
		b[i] = '0'
	}
	return string(b)
}

// pendingSpend reads the stored d-{nonce} intent fields exactly (amounts are
// JSON numbers wider than float64).
func pendingSpend(raw string) (nonce uint64, to, amount, asset string, err error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return 0, "", "", "", err
	}
	n, _ := strconv.ParseUint(fmt.Sprint(m["nonce"]), 10, 64)
	return n, fmt.Sprint(m["to"]), fmt.Sprint(m["amount"]), fmt.Sprint(m["asset"]), nil
}

var opTxRe = regexp.MustCompile(`submitProof tx (\S+)`)

func TestEVMBridge_WReal_SepoliaProofs(t *testing.T) {
	if testing.Short() {
		t.Skip("devnet")
	}
	requireDocker(t)
	ctx, cancel := devnetWithTimeout(context.Background(), 400*time.Minute)
	t.Cleanup(cancel)

	execRPC := envOr("WREAL_EXEC_RPC", wrealExecRPC)
	beaconRPC := envOr("WREAL_BEACON_RPC", wrealBeaconRPC)
	mainnetRPC := envOr("WREAL_MAINNET_RPC", wrealMainnetRPC)
	logDir := envOr("WREAL_LOG_DIR", t.TempDir())
	signerDir := os.Getenv("ZK_TX_SIGNER_DIR")
	if _, err := os.Stat(filepath.Join(signerDir, "zk-tx-signer")); err != nil {
		t.Fatalf("ZK_TX_SIGNER_DIR=%q has no zk-tx-signer: %v", signerDir, err)
	}
	walletKey, _ := readHexKey(t, "WREAL_SEPOLIA_KEY_FILE")
	_, requesterHex := readHexKey(t, "WREAL_REQUESTER_KEY_FILE")
	wallet := cryptoPubkeyAddr(walletKey)
	usdcAddr := ethcommon.HexToAddress(wrealUSDC)
	milliEth := func(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(1e15)) }
	// The user deposit's 1% tax tops the reserve deposit up to exactly the
	// 0.05 ETH floor (MinGasReserve) that every withdrawal needs.
	userDep := milliEth(25)
	reserveDep := new(big.Int).Sub(milliEth(50), new(big.Int).Div(userDep, big.NewInt(100)))
	smallDep := milliEth(1) // F15 (twice) and F16

	// Funds first: an unfunded wallet must not cost a devnet.
	l1 := dialSepolia(t, ctx, execRPC)
	pr := l1.proofs(execRPC)
	ethBal, err := l1.cli.BalanceAt(ctx, wallet, nil)
	if err != nil {
		t.Fatal(err)
	}
	usdcBal, err := l1.usdcBalance(ctx, wallet)
	if err != nil {
		t.Fatal(err)
	}
	needEth := new(big.Int).Add(reserveDep, userDep)
	needEth.Add(needEth, new(big.Int).Mul(smallDep, big.NewInt(3)))
	needEth.Add(needEth, big.NewInt(600_000_000_000_000)) // + 0.0006 ETH of gas
	if ethBal.Cmp(needEth) < 0 || usdcBal.Cmp(big.NewInt(2_000_000)) < 0 {
		t.Fatalf("Sepolia wallet %s holds %s wei and %s USDC units; needs %s wei and 2000000", wallet.Hex(), ethBal, usdcBal, needEth)
	}
	t.Logf("Sepolia wallet %s: %s wei, %s USDC units", wallet.Hex(), ethBal, usdcBal)

	// One 5-node devnet at the 0.13 floor, its image built from WREAL_SOURCE_DIR
	// when set (a tree identical to the suite run reuses its image layers).
	cfg := evmBridgeConfig()
	if src := os.Getenv("WREAL_SOURCE_DIR"); src != "" {
		cfg.SourceDir = src
	}
	cp := cfg.SysConfigOverrides.ConsensusParams
	cp.ConsensusVersionFloorMajor, cp.ConsensusVersionFloorConsensus, cp.ConsensusVersionFloorEpoch = 0, 13, 1
	d, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Start(ctx); err != nil {
		t.Fatalf("devnet start: %v", err)
	}
	t.Cleanup(func() { _ = d.Stop() })
	if err := bootstrapEvmOwner(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := waitForConsensus(ctx, d, "0.13", 20*time.Minute); err != nil {
		t.Fatal(err)
	}

	// M1: a committee weighted like mainnet's. Every witness starts at 2000;
	// node 4 becomes the heaviest (v4vapp's role), node 1 (the owner) the lightest.
	extra := map[int]int64{4: 4000, 3: 3400, 2: 2000, 5: 600}
	for n, amt := range extra {
		a := fmt.Sprintf("%d.000", amt)
		if _, err := d.Deposit(ctx, n, a, "hive"); err != nil {
			t.Errorf("M1: deposit %s hive for node %d: %v", a, n, err)
			continue
		}
		acct := "hive:" + d.witnessAccount(n)
		if err := pollUntil(ctx, 6*time.Minute, func() bool {
			b, err := d.GetAccountBalance(ctx, 1, acct)
			return err == nil && b != nil && b.Hive >= amt*1000
		}); err != nil {
			t.Errorf("M1: %s hive never credited to %s", a, acct)
			continue
		}
		if _, err := d.ConsensusStake(n, n, a); err != nil {
			t.Errorf("M1: consensus stake %s for node %d: %v", a, n, err)
		}
	}
	currentEpoch := func() uint64 {
		var out struct {
			E struct {
				Epoch uint64 `json:"epoch"`
			} `json:"electionByBlockHeight"`
		}
		_ = d.gqlQuery(ctx, 1, `{ electionByBlockHeight { epoch } }`, nil, &out)
		return out.E.Epoch
	}
	heaviest := func(el *ElectionInfo) (string, float64) {
		best, bw := "", uint64(0)
		for i, m := range el.Members {
			if i < len(el.Weights) && el.Weights[i] > bw {
				best, bw = m, el.Weights[i]
			}
		}
		if el.TotalWeight == 0 {
			return best, 0
		}
		return best, float64(bw) / float64(el.TotalWeight)
	}
	var weighted *ElectionInfo
	if err := pollUntil(ctx, 15*time.Minute, func() bool {
		el, err := d.GetElectionGQL(ctx, 1, currentEpoch())
		if err != nil || el == nil {
			return false
		}
		who, share := heaviest(el)
		weighted = el
		return who == d.witnessAccount(4) && share > 0.25
	}); err != nil {
		t.Errorf("M1 NOT APPLIED: the election never weighted node 4 heaviest: %+v", weighted)
	} else {
		t.Logf("M1 PASS: mainnet-shaped committee in epoch %d: members %v weights %v (total %d)", weighted.Epoch, weighted.Members, weighted.Weights, weighted.TotalWeight)
	}

	e := &evmBridgeEnv{t: t, ctx: ctx, d: d, ownerNode: 1, gqlNode: 2, propID: 1, ownerAcct: "hive:magi.test1"}
	e.amID, err = deployWithRetry(ctx, d, ContractDeployOpts{WasmPath: evmArtifact(t, "EVM_BRIDGE_DEVNET_WASM"), Name: "evm-bridge", DeployerNode: 1, GQLNode: 2}, 3)
	if err != nil {
		t.Fatalf("deploy account-mapping: %v", err)
	}
	verifierID, err := deployWithRetry(ctx, d, ContractDeployOpts{WasmPath: evmArtifact(t, "ZK_VERIFIER_WASM"), Name: "zk-header-verifier", DeployerNode: 1, GQLNode: 2}, 3)
	if err != nil {
		t.Fatalf("deploy zk verifier: %v", err)
	}
	if _, err := d.CallContract(ctx, 1, e.amID, "initContract", evmInitPayload); err != nil {
		t.Fatalf("init account-mapping: %v", err)
	}
	if err := e.proposeExecute("setVerifierContract", fmt.Sprintf(`{"contract_id":"%s"}`, verifierID)); err != nil {
		t.Fatalf("wire verifier: %v", err)
	}
	if err := pollUntil(ctx, evmL2Wait, func() bool { return e.stateStr("zkverifier") == verifierID }); err != nil {
		t.Fatalf("setVerifierContract did not take effect")
	}
	vStateOf := func(id string, keys ...string) map[string]string {
		m, _ := d.GetStateByKeys(ctx, 2, id, keys)
		out := map[string]string{}
		for _, k := range keys {
			if m != nil && m[k] != nil {
				out[k] = fmt.Sprintf("%v", m[k])
			}
		}
		return out
	}
	vState := func(keys ...string) map[string]string { return vStateOf(verifierID, keys...) }
	t.Logf("contracts: account-mapping=%s, real zk verifier=%s (wired)", e.amID, verifierID)

	// USDC: Circle's Sepolia token.
	reg := fmt.Sprintf(`{"address":"%s","symbol":"usdc","decimals":6,"min_withdrawal":1000000}`, usdcAddr.Hex())
	if err := e.proposeExecute("registerToken", reg); err != nil {
		t.Fatalf("registerToken usdc: %v", err)
	}
	regKey := "t-" + strconv.FormatUint(evmChainID, 10) + "-" + strings.ToLower(usdcAddr.Hex()[2:])
	if err := pollUntil(ctx, evmL2Wait, func() bool { return e.stateStr(regKey) == "usdc|6|1000000" }); err != nil {
		t.Fatalf("registerToken not stored: %s=%q", regKey, e.stateStr(regKey))
	}

	// Relayers: the bot, a second honest bot (M3), and a malicious relayer
	// that submits every forged call.
	botKey, botBKey, attKey := mustKey(t), mustKey(t), mustKey(t)
	botDID := dids.NewEthDID(cryptoPubkeyAddr(botKey).Hex()).String()
	botBDID := dids.NewEthDID(cryptoPubkeyAddr(botBKey).Hex()).String()
	attDID := dids.NewEthDID(cryptoPubkeyAddr(attKey).Hex()).String()
	for _, r := range []string{botDID, botBDID, attDID} {
		if err := e.proposeExecute("registerRelayer", fmt.Sprintf(`{"account":"%s"}`, r)); err != nil {
			t.Fatalf("registerRelayer %s: %v", r, err)
		}
		if err := e.fundDID(r, "20000.000"); err != nil {
			t.Fatalf("fund %s: %v", r, err)
		}
	}
	Katt, err := NewEthKey(hex.EncodeToString(ethCrypto.FromECDSA(attKey)))
	if err != nil {
		t.Fatal(err)
	}
	// attempt submits one call as the malicious relayer and returns its outcome.
	attempt := func(contract, action, payload string) (string, string) {
		tx, err := d.CallContractEvm(ctx, 2, Katt, contract, action, payload, 5000000)
		if err != nil {
			return "NOT-BROADCAST", err.Error()
		}
		var st string
		_ = pollUntil(ctx, evmL2Wait, func() bool {
			st, _ = d.FindTransactionStatus(ctx, 2, tx)
			return st == "FAILED" || st == "CONFIRMED"
		})
		out, _ := e.outResultViaDAG(ctx, tx)
		return st, out.msg
	}
	refused := func(id, contract, action, payload, want string) bool {
		t.Helper()
		st, msg := attempt(contract, action, payload)
		if st != "FAILED" || (want != "" && !strings.Contains(msg, want)) {
			t.Errorf("%s FAIL: %s was %s (%q); want FAILED with %q", id, action, st, msg, want)
			return false
		}
		t.Logf("%s PASS: %s refused: %s", id, action, msg)
		return true
	}

	// (0) The vault, attested by the real bot.
	e.botAttests = true
	vault, err := e.tssVault(25 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// RP-1: a bot without a checkpoint scans from L1 block 1 (36 days on
	// mainnet). Seed it at the current finalized block, before any deposit.
	seed, err := l1.finalized(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cpPath := filepath.Join(logDir, "bot-checkpoint.json")
	seedCheckpoint := func(path string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(fmt.Sprintf(`{"last_scanned_block": %d}`, seed)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	seedCheckpoint(cpPath)
	botHex := hex.EncodeToString(ethCrypto.FromECDSA(botKey))
	bot := StartBot(t, ctx, BotConfig{
		EthRPC: execRPC, ContractID: e.amID, GraphQLURL: d.GQLEndpoint(2),
		EthPrivHex: botHex, Checkpoint: cpPath, Tokens: usdcAddr.Hex(),
	})
	t.Cleanup(bot.Stop)
	botLogs := func(b *Bot) string { return strings.Join(b.Logs(), "\n") }
	if !bot.WaitForLog(ctx, "E23: attested the vault key's check signature", evmL2Wait+10*time.Minute) {
		t.Fatalf("the bot never attested the vault key:\n%s", botLogs(bot))
	}
	if !bot.WaitForLog(ctx, "A18: vault is the TSS key's address", evmL2Wait) {
		t.Fatalf("the bot did not take the vault:\n%s", botLogs(bot))
	}
	t.Logf("(0) PASS: vault %s attested and live; bot checkpoint seeded at Sepolia block %d", vault.Hex(), seed)
	// M3: a second honest bot with its own relayer key, overlapping the first.
	cpB := filepath.Join(logDir, "botB-checkpoint.json")
	seedCheckpoint(cpB)
	botB := StartBot(t, ctx, BotConfig{
		EthRPC: execRPC, ContractID: e.amID, GraphQLURL: d.GQLEndpoint(3),
		EthPrivHex: hex.EncodeToString(ethCrypto.FromECDSA(botBKey)), Checkpoint: cpB, Tokens: usdcAddr.Hex(),
	})
	t.Cleanup(botB.Stop)
	// M2: the vault key and the epoch, tracked through the run.
	vaultPub, epoch0 := e.getTssPubKey(), currentEpoch()
	checkVaultKey := func(where string) {
		t.Helper()
		if pub := e.getTssPubKey(); pub != vaultPub {
			t.Errorf("M2 FAIL at %s: the vault key changed %s -> %s (epoch %d -> %d)", where, vaultPub, pub, epoch0, currentEpoch())
			return
		}
		t.Logf("M2 at %s: epoch %d -> %d, vault key unchanged", where, epoch0, currentEpoch())
	}

	// (1) Verifier init at the live finalized checkpoint, then the ELF pin.
	gout, err := exec.CommandContext(ctx, evmArtifact(t, "MAGI_GENESIS_BIN"),
		"--source-chain-id", strconv.FormatUint(evmChainID, 10),
		"--source-consensus-rpc", beaconRPC, "--source-execution-rpc", execRPC, "--is-testnet").Output()
	if err != nil {
		t.Fatalf("magi_genesis: %v", err)
	}
	// UseNumber: chain id and heights must stay exact integers (a float64
	// prints 11155111 as 1.1155111e+07).
	decodeExact := func(b []byte, v *map[string]any) error {
		dec := json.NewDecoder(strings.NewReader(string(b)))
		dec.UseNumber()
		return dec.Decode(v)
	}
	var genesis map[string]any
	if err := decodeExact(gout, &genesis); err != nil {
		t.Fatalf("magi_genesis output: %v\n%s", err, gout)
	}
	if fmt.Sprint(genesis["expected_chain_id"]) != "11155111" || genesis["is_testnet"] != true {
		t.Fatalf("magi_genesis payload not for Sepolia testnet: %s", gout)
	}
	if err := os.WriteFile(filepath.Join(logDir, "genesis.json"), gout, 0o644); err != nil {
		t.Fatal(err)
	}
	anchor, err := strconv.ParseUint(fmt.Sprint(genesis["initial_height"]), 10, 64)
	if err != nil {
		t.Fatalf("magi_genesis initial_height %v: %v", genesis["initial_height"], err)
	}
	initVerifier := func(id, payload string) {
		t.Helper()
		if _, err := e.callOnce(1, id, "init", payload); err != nil {
			t.Fatalf("init verifier %s: %v", id, err)
		}
		if err := pollUntil(ctx, evmL2Wait, func() bool { return vStateOf(id, "h")["h"] == strconv.FormatUint(anchor, 10) }); err != nil {
			t.Fatalf("verifier %s init did not land: %v", id, vStateOf(id, "h", "bh"))
		}
		if _, err := e.callOnce(1, id, "setExpectedElfHash", fmt.Sprintf(`{"hash":"%s"}`, wrealElfHash)); err != nil {
			t.Fatalf("setExpectedElfHash %s: %v", id, err)
		}
		if err := pollUntil(ctx, evmL2Wait, func() bool { return strings.Contains(vStateOf(id, "elfhash")["elfhash"], wrealElfHash) }); err != nil {
			t.Fatalf("ELF pin did not land on %s: %v", id, vStateOf(id, "elfhash"))
		}
	}
	initVerifier(verifierID, strings.TrimSpace(string(gout)))
	t.Logf("(1) PASS: verifier init at slot %v / block %d (%v), ELF pinned", genesis["initial_head"], anchor, genesis["initial_block_hash"])

	// (2) Sepolia deposits.
	rcRes, err := l1.send(ctx, walletKey, vault, reserveDep, []byte("to_reserve=1"), 30_000)
	if err != nil || rcRes.Status != 1 {
		t.Fatalf("reserve deposit: %v %+v", err, rcRes)
	}
	rcEth, err := l1.send(ctx, walletKey, vault, userDep, nil, 21_000)
	if err != nil || rcEth.Status != 1 {
		t.Fatalf("ETH deposit: %v %+v", err, rcEth)
	}
	rcUsdc, err := l1.send(ctx, walletKey, usdcAddr, new(big.Int), usdcCall("transfer(address,uint256)", vault, big.NewInt(2_000_000)), 100_000)
	if err != nil || rcUsdc.Status != 1 {
		t.Fatalf("USDC deposit: %v %+v", err, rcUsdc)
	}
	lastDeposit := rcUsdc.BlockNumber.Uint64()
	t.Logf("(2) deposits on Sepolia: reserve %s (block %d), ETH %s (block %d), USDC %s (block %d)",
		rcRes.TxHash.Hex(), rcRes.BlockNumber, rcEth.TxHash.Hex(), rcEth.BlockNumber, rcUsdc.TxHash.Hex(), lastDeposit)

	// (3) T1: the first paid proof, once the deposits are finalized.
	opKey := mustKey(t)
	opDID := dids.NewEthDID(cryptoPubkeyAddr(opKey).Hex()).String()
	if err := e.fundDID(opDID, "20000.000"); err != nil {
		t.Fatalf("fund operator DID: %v", err)
	}
	opEnvWith := func(requester string) []string {
		return append(os.Environ(),
			"NETWORK_PRIVATE_KEY="+requester,
			"MAGI_OPERATOR_PRIVATE_KEY="+hex.EncodeToString(ethCrypto.FromECDSA(opKey)),
			"SP1_HELIOS_FULFILLMENT_STRATEGY=auction",
			"PATH="+signerDir+":"+os.Getenv("PATH"),
			"RUST_LOG=info",
		)
	}
	opEnv := opEnvWith(requesterHex)
	opArgsFor := func(verifier, exec string, maxProofs int) []string {
		return []string{
			"--source-chain-id", strconv.FormatUint(evmChainID, 10),
			"--source-consensus-rpc", beaconRPC, "--source-execution-rpc", exec,
			"--magi-graphql", d.GQLEndpoint(2), "--verifier-contract", verifier,
			"--net-id", d.netId(), "--land-timeout-mins", "5", "--max-proofs", strconv.Itoa(maxProofs),
		}
	}
	logOp := func(name, out string) {
		for _, l := range operatorLines(out, "identity", "ELF hash", "proof budget", "received from the Prover Network", "submitProof", "submitHeaders", "landed", "stopping", "refus", "mismatch", "rror", "PGU", "gas", "cost", "price", "cycles", "fee", "auction", "Request") {
			t.Logf("  op %s| %s", name, l)
		}
	}
	// checkProof asserts a paid proof landed and its gap is closed.
	checkProof := func(name string, before map[string]string, mustCover uint64) {
		t.Helper()
		after := vState("bh", "h", "gapb", "gapw")
		h, _ := strconv.ParseUint(after["h"], 10, 64)
		if after["bh"] == before["bh"] || h < mustCover || after["gapb"] != "" || after["gapw"] != "" {
			t.Fatalf("%s: verifier state after the proof %v (before %v): want bh moved, h >= %d, no open gap", name, after, before, mustCover)
		}
		t.Logf("%s PASS: a real network proof was accepted on-chain; beacon head %s -> %s, headers to block %s, gap closed", name, before["bh"], after["bh"], after["h"])
	}
	if err := l1.waitFinalized(ctx, lastDeposit, 40*time.Minute); err != nil {
		t.Fatal(err)
	}
	before := vState("bh", "h")
	out, _, err := runOperator(t, ctx, filepath.Join(logDir, "operator-T1.log"), opEnv, opArgsFor(verifierID, execRPC, 1), 100*time.Minute, nil)
	logOp("T1", out)
	if err != nil {
		t.Fatalf("T1: operator exited with %v", err)
	}
	if len(operatorLines(out, "proof 1 received from the Prover Network")) != 1 || len(operatorLines(out, "1 proof(s) landed")) == 0 {
		t.Fatalf("T1: the operator log does not show exactly one paid proof that landed")
	}
	checkProof("T1", before, lastDeposit)
	// RP-9: the operator's RC for one proof plus its backfill (its first txs,
	// so everything frozen now is T1's; regen over ~minutes is ~0.1%).
	var rc struct {
		R struct {
			Amount int64 `json:"amount"`
			Max    int64 `json:"max_rcs"`
		} `json:"getAccountRC"`
	}
	if err := d.gqlQuery(ctx, 2, `query($a:String!){getAccountRC(account:$a){amount max_rcs}}`, map[string]any{"a": opDID}, &rc); err == nil && rc.R.Max > 0 {
		hT1, _ := strconv.ParseUint(vState("h")["h"], 10, 64)
		headers := int64(hT1 - anchor)
		frozen := rc.R.Max - rc.R.Amount
		if headers > 0 {
			t.Logf("RP-9 MEASURED: T1 froze %d RC on the operator for %d headers (proof + backfill): %.0f RC per header; x7200 headers/day = %.1fM RC/day",
				frozen, headers, float64(frozen)/float64(headers), float64(frozen)/float64(headers)*7200/1e6)
		}
	} else {
		t.Logf("RP-9 NOT MEASURED: getAccountRC(%s): %v %+v", opDID, err, rc.R)
	}

	// F1-F3 on T1's real proof (no gap is open, so each reaches the checks).
	if m := opTxRe.FindStringSubmatch(strings.Join(operatorLines(out, "submitProof tx"), "\n")); m == nil {
		t.Errorf("F1-F3 NOT RUN: no submitProof tx id in the T1 log")
	} else if raw, err := callPayload(ctx, d, m[1]); err != nil {
		t.Errorf("F1-F3 NOT RUN: %v", err)
	} else {
		var p map[string]any
		if err := json.Unmarshal([]byte(raw), &p); err != nil || p["proof"] == nil || p["public_values"] == nil {
			t.Errorf("F1-F3 NOT RUN: T1 payload is not a submitProof payload: %.200s", raw)
		} else {
			with := func(k, v string) string {
				q := map[string]any{}
				for kk, vv := range p {
					q[kk] = vv
				}
				q[k] = v
				b, _ := json.Marshal(q)
				return string(b)
			}
			bh := vState("bh")["bh"]
			refused("F1", verifierID, "submitProof", with("proof", flipHex(fmt.Sprint(p["proof"]))), "proof verification failed")
			refused("F3", verifierID, "submitProof", with("public_values", flipHex(fmt.Sprint(p["public_values"]))), "proof verification failed")
			if st, msg := attempt(verifierID, "submitProof", raw); st != "FAILED" || strings.Contains(msg, "verification failed") {
				t.Errorf("F2 FAIL: the replayed real proof was %s (%q); want FAILED after the Groth16 check (head not advancing)", st, msg)
			} else {
				t.Logf("F2 PASS: the real proof replayed is refused past the Groth16 check: %s", msg)
			}
			if vState("bh")["bh"] != bh {
				t.Errorf("F1-F3 FAIL: the beacon head moved during the forged submissions: %s -> %s", bh, vState("bh")["bh"])
			}
		}
	}

	// F13: a mainnet execution RPC for this Sepolia verifier: no paid proof.
	noPaidProof := func(id, logName string, env, args []string, within time.Duration) {
		t.Helper()
		out, stopped, err := runOperator(t, ctx, filepath.Join(logDir, logName), env, args, within,
			func(l string) bool { return strings.Contains(l, "operator step failed") })
		logOp(id, out)
		if len(operatorLines(out, "received from the Prover Network")) != 0 {
			t.Errorf("%s FAIL: the operator was paid for a proof (log %s)", id, logName)
			return
		}
		t.Logf("%s PASS: no proof bought (exit %v, stopped on a failed step: %v)", id, err, stopped)
	}
	noPaidProof("F13", "operator-F13-mainnet-rpc.log", opEnv, opArgsFor(verifierID, mainnetRPC, 1), 15*time.Minute)
	// F9: a requester with no PROVE: no proof, no spend.
	noPaidProof("F9", "operator-F9-empty-requester.log", opEnvWith(hex.EncodeToString(ethCrypto.FromECDSA(mustKey(t)))), opArgsFor(verifierID, execRPC, 1), 15*time.Minute)
	// F7: a verifier whose vkey is not this ELF's: refused before paying.
	if v2, err := deployWithRetry(ctx, d, ContractDeployOpts{WasmPath: evmArtifact(t, "ZK_VERIFIER_WASM"), Name: "zk-header-verifier-f7", DeployerNode: 1, GQLNode: 2}, 3); err != nil {
		t.Errorf("F7 NOT RUN: deploy: %v", err)
	} else {
		g2 := map[string]any{}
		_ = decodeExact(gout, &g2)
		g2["sp1_vkey_hash"] = flipHex(fmt.Sprint(g2["sp1_vkey_hash"]))
		b, _ := json.Marshal(g2)
		initVerifier(v2, string(b))
		noPaidProof("F7", "operator-F7-vkey-mismatch.log", opEnv, opArgsFor(v2, execRPC, 1), 15*time.Minute)
	}

	// (4) The bot maps the three deposits from the proven headers.
	user := evmDID(wallet)
	tax := new(big.Int).Div(userDep, big.NewInt(100))
	wantUser, wantGr := new(big.Int).Sub(userDep, tax), new(big.Int).Add(reserveDep, tax) // gr = exactly 0.05
	if err := pollUntil(ctx, 15*time.Minute, func() bool {
		return e.stateStr("a-"+user+"-eth") == wantUser.String() && e.stateStr("a-"+user+"-usdc") == "2000000" && e.stateStr("gr") == wantGr.String()
	}); err != nil {
		t.Fatalf("deposits not mapped: eth=%q usdc=%q gr=%q (want %s, 2000000, %s)\nbot:\n%s",
			e.stateStr("a-"+user+"-eth"), e.stateStr("a-"+user+"-usdc"), e.stateStr("gr"), wantUser, wantGr, botLogs(bot))
	}
	if got := e.stateStr("s-usdc"); got != "2000000|2000000|0|0" {
		t.Fatalf("s-usdc=%q after the deposit", got)
	}
	t.Logf("(4) PASS: mapped from real proofs: %s eth=%s usdc=2000000, gas reserve %s", user, wantUser, wantGr)

	// F5 / F11: forged and replayed deposits from the malicious relayer.
	balances := func() string {
		return e.stateStr("a-"+user+"-eth") + "|" + e.stateStr("a-"+user+"-usdc") + "|" + e.stateStr("gr")
	}
	snap := balances()
	depBlock := rcEth.BlockNumber.Uint64()
	mapPayload := func(block, idx uint64, rawHex, proofHex string) string {
		return fmt.Sprintf(`{"tx_data":{"block_height":%d,"tx_index":%d,"raw_hex":"%s","merkle_proof_hex":"%s","deposit_type":"eth"},"instructions":[]}`, block, idx, rawHex, proofHex)
	}
	if depIdx, err := pr.FindTxIndex(ctx, depBlock, rcEth.TxHash); err != nil {
		t.Errorf("F5/F11 NOT RUN: deposit index: %v", err)
	} else if rawHex, proofHex, err := pr.TxTrieProof(ctx, depBlock, depIdx); err != nil {
		t.Errorf("F5/F11 NOT RUN: trie proof (geth decoding of a Sepolia block?): %v", err)
	} else {
		refused("F5-replay", e.amID, "map", mapPayload(depBlock, depIdx, rawHex, proofHex), "")
		h, _ := strconv.ParseUint(vState("h")["h"], 10, 64)
		refused("F11", e.amID, "map", mapPayload(h+1000, depIdx, rawHex, proofHex), "")
		// Another tx in the same proven block, its real proof, and a raw tx
		// signed by the user that pays the vault 1 ETH.
		other := uint64(0)
		if other == depIdx {
			other = 1
		}
		if _, otherProof, err := pr.TxTrieProof(ctx, depBlock, other); err != nil {
			t.Errorf("F5-forge NOT RUN: %v", err)
		} else {
			forged, _ := ethtypes.SignTx(ethtypes.NewTx(&ethtypes.DynamicFeeTx{ChainID: l1.chainID, Nonce: 1 << 20, GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(20e9), Gas: 21000, To: &vault, Value: big.NewInt(1e18)}),
				ethtypes.LatestSignerForChainID(l1.chainID), walletKey)
			fb, _ := forged.MarshalBinary()
			refused("F5-forge", e.amID, "map", mapPayload(depBlock, other, hex.EncodeToString(fb), otherProof), "")
		}
		refused("F5-tamper", e.amID, "map", mapPayload(depBlock, depIdx, flipHex(rawHex), proofHex), "")
	}
	devnetSleep(30 * time.Second)
	if got := balances(); got != snap {
		t.Errorf("F5/F11 FAIL: forged or replayed maps moved balances: %s -> %s", snap, got)
	}

	if os.Getenv("WREAL_STOP_AFTER") == "T1" {
		t.Logf("WREAL_STOP_AFTER=T1: stopping after T1, mapping and the forgery/operator checks")
		return
	}

	// (5a) Withdrawals go one at a time (RP-11). The ETH one first, with node 4
	// (the heaviest, ~30% of the weight) down while the TSS signs it (F12).
	if err := e.fundDID(user, "10000.000"); err != nil {
		t.Fatalf("fund user DID: %v", err)
	}
	K, err := NewEthKey(hex.EncodeToString(ethCrypto.FromECDSA(walletKey)))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.StopNode(ctx, 4); err != nil {
		t.Errorf("F12 NOT RUN: stop node 4: %v", err)
	}
	recipU, recipE := cryptoPubkeyAddr(mustKey(t)), cryptoPubkeyAddr(mustKey(t))
	ethUnmap := fmt.Sprintf(`{"to":"%s","amount":"%s","asset":"eth"}`, recipE.Hex(), milliEth(10))
	usdcUnmap := fmt.Sprintf(`{"to":"%s","amount":"1000000","token_address":"%s"}`, recipU.Hex(), usdcAddr.Hex())
	stored := map[uint64]string{}
	queue := func(action, payload string, nonce uint64) *ethtypes.DynamicFeeTx {
		t.Helper()
		if _, err := d.CallContractEvm(ctx, 2, K, e.amID, action, payload, 5000000); err != nil {
			t.Fatalf("%s: %v", action, err)
		}
		var ps struct {
			UnsignedTxHex string `json:"unsigned_tx_hex"`
		}
		key := "d-" + strconv.FormatUint(nonce, 10)
		if err := pollUntil(ctx, evmL2Wait, func() bool {
			return json.Unmarshal([]byte(e.stateStr(key)), &ps) == nil && ps.UnsignedTxHex != ""
		}); err != nil {
			t.Fatalf("%s queued nothing at %s (%q)", action, key, e.stateStr(key))
		}
		stored[nonce] = e.stateStr(key)
		q, err := DecodeUnsigned1559(ps.UnsignedTxHex)
		if err != nil {
			t.Fatal(err)
		}
		return q
	}
	// userCall submits a call as the user and returns its settled outcome.
	userCall := func(action, payload string) (string, string) {
		tx, err := d.CallContractEvm(ctx, 2, K, e.amID, action, payload, 5000000)
		if err != nil {
			return "NOT-BROADCAST", err.Error()
		}
		var st string
		_ = pollUntil(ctx, evmL2Wait, func() bool {
			st, _ = d.FindTransactionStatus(ctx, 2, tx)
			return st == "FAILED" || st == "CONFIRMED"
		})
		o, _ := e.outResultViaDAG(ctx, tx)
		return st, o.msg
	}
	type sent struct {
		tx    *ethtypes.Transaction
		rc    *ethtypes.Receipt
		block uint64
	}
	vaultSent := map[uint64]sent{}
	waitSent := func(nonce uint64, q *ethtypes.DynamicFeeTx, from uint64, logs func() string) sent {
		t.Helper()
		next := from
		var tx *ethtypes.Transaction
		var rc *ethtypes.Receipt
		if err := pollUntil(ctx, 30*time.Minute, func() bool {
			var ok bool
			tx, rc, ok = l1.vaultTx(ctx, vault, nonce, &next)
			return ok
		}); err != nil {
			t.Fatalf("the vault never sent nonce %d on Sepolia\nbot:\n%s", nonce, logs())
		}
		if rc.Status != 1 || tx.To() == nil || *tx.To() != *q.To || tx.Value().Cmp(q.Value) != 0 || !strings.EqualFold(hex.EncodeToString(tx.Data()), hex.EncodeToString(q.Data)) {
			t.Fatalf("vault nonce %d: status %d to=%v value=%s, want the queued tx to=%v value=%s", nonce, rc.Status, tx.To(), tx.Value(), q.To, q.Value)
		}
		s := sent{tx, rc, rc.BlockNumber.Uint64()}
		vaultSent[nonce] = s
		t.Logf("vault nonce %d mined on Sepolia: %s block %d gas %d", nonce, tx.Hash().Hex(), s.block, rc.GasUsed)
		return s
	}
	scanFrom, err := l1.cli.BlockNumber(ctx)
	if err != nil {
		t.Fatal(err)
	}
	qE := queue("unmapETH", ethUnmap, 0)
	checkVaultKey("ETH withdrawal queued")
	// RP-11 on real data: while d-0 is pending, the USDC withdrawal is refused.
	if st, msg := userCall("unmapERC20", usdcUnmap); st != "FAILED" || !strings.Contains(msg, "withdrawal pending") || e.stateStr("d-1") != "" {
		t.Errorf("RP-11 observation: a second withdrawal while d-0 is pending was %s (%q), d-1 queued=%v", st, msg, e.stateStr("d-1") != "")
	} else {
		t.Logf("RP-11 OBSERVED on real data: with d-0 pending, the user's USDC withdrawal is refused: %s", msg)
	}
	sE := waitSent(0, qE, scanFrom, func() string { return botLogs(bot) })
	if got, _ := l1.cli.BalanceAt(ctx, recipE, nil); got == nil || got.Cmp(qE.Value) != 0 {
		t.Fatalf("ETH recipient holds %v wei, want %s", got, qE.Value)
	}
	afterE := map[string]string{"eth": e.stateStr("a-" + user + "-eth"), "usdc": e.stateStr("a-" + user + "-usdc")}
	t.Logf("(5a) PASS: the TSS-signed ETH withdrawal paid %s wei on Sepolia with node 4 down (F12); user now eth=%s", qE.Value, afterE["eth"])
	if err := d.StartNode(ctx, 4); err != nil {
		t.Errorf("F12 FAIL: restart node 4: %v", err)
	}

	// F15: legacy and access-list deposits; F16: a deposit forwarded by a
	// contract's creation code (an internal tx to the vault).
	rcLegacy, err := l1.sendTx(ctx, walletKey, func(n uint64) ethtypes.TxData {
		return &ethtypes.LegacyTx{Nonce: n, GasPrice: big.NewInt(2e9), Gas: 21000, To: &vault, Value: smallDep}
	})
	if err != nil || rcLegacy.Status != 1 {
		t.Errorf("F15 NOT RUN: legacy deposit: %v %+v", err, rcLegacy)
	}
	rcAL, err := l1.sendTx(ctx, walletKey, func(n uint64) ethtypes.TxData {
		return &ethtypes.AccessListTx{ChainID: l1.chainID, Nonce: n, GasPrice: big.NewInt(2e9), Gas: 21000, To: &vault, Value: smallDep}
	})
	if err != nil || rcAL.Status != 1 {
		t.Errorf("F15 NOT RUN: access-list deposit: %v %+v", err, rcAL)
	}
	// init code: CALL(gas, vault, callvalue, 0, 0, 0, 0); STOP (deploys no code).
	initCode := append(append(ethcommon.FromHex("0x600060006000600034"+"73"), vault.Bytes()...), ethcommon.FromHex("0x5af15000")...)
	vaultBefore, _ := l1.cli.BalanceAt(ctx, vault, nil)
	rcFwd, err := l1.sendTx(ctx, walletKey, func(n uint64) ethtypes.TxData {
		return &ethtypes.DynamicFeeTx{ChainID: l1.chainID, Nonce: n, GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(20e9), Gas: 120_000, Value: smallDep, Data: initCode}
	})
	fwdOK := err == nil && rcFwd.Status == 1
	if vaultAfter, _ := l1.cli.BalanceAt(ctx, vault, nil); !fwdOK || vaultBefore == nil || vaultAfter == nil || new(big.Int).Sub(vaultAfter, vaultBefore).Cmp(smallDep) != 0 {
		t.Errorf("F16 NOT RUN: the forwarding creation tx did not move %s wei to the vault (%v %+v)", smallDep, err, rcFwd)
		fwdOK = false
	}
	lastT2 := sE.block
	for _, rc := range []*ethtypes.Receipt{rcLegacy, rcAL, rcFwd} {
		if rc != nil && rc.BlockNumber.Uint64() > lastT2 {
			lastT2 = rc.BlockNumber.Uint64()
		}
	}

	// (6a) T2, with F8: the operator is killed once backfill starts.
	bot.Stop() // the confirming bot comes later, from this checkpoint (F10)
	botB.Stop()
	if err := l1.waitFinalized(ctx, lastT2, 40*time.Minute); err != nil {
		t.Fatal(err)
	}
	before = vState("bh", "h")
	out, killed, _ := runOperator(t, ctx, filepath.Join(logDir, "operator-T2.log"), opEnv, opArgsFor(verifierID, execRPC, 1), 100*time.Minute,
		func(l string) bool { return strings.Contains(l, "submitHeaders tx") })
	logOp("T2", out)
	if len(operatorLines(out, "proof 1 received from the Prover Network")) != 1 {
		t.Fatalf("T2: the operator log does not show exactly one paid proof")
	}
	devnetSleep(30 * time.Second) // the killed operator's last submitHeaders lands
	gap := vState("gapw", "gapl")
	if !killed || gap["gapw"] == "" {
		t.Errorf("F8/F4 NOT EXERCISED: killed=%v, gap %v (the backfill finished before the kill)", killed, gap)
	} else {
		// F4: the next header the gap wants, from Sepolia; changed, then real.
		low, _ := strconv.ParseUint(gap["gapl"], 10, 64)
		hdr, err := l1.cli.HeaderByNumber(ctx, new(big.Int).SetUint64(low-1))
		if err != nil {
			t.Errorf("F4 NOT RUN: header %d: %v", low-1, err)
		} else if enc, err := rlp.EncodeToBytes(hdr); err != nil || ethCrypto.Keccak256Hash(enc) != hdr.Hash() ||
			!strings.EqualFold(strings.TrimPrefix(gap["gapw"], "0x"), hex.EncodeToString(hdr.Hash().Bytes())) {
			t.Errorf("F4 NOT RUN: local RLP of block %d does not reproduce the wanted hash %s (%v)", low-1, gap["gapw"], err)
		} else {
			bad := ethtypes.CopyHeader(hdr)
			bad.Extra = append(append([]byte{}, bad.Extra...), 0x01)
			badEnc, _ := rlp.EncodeToBytes(bad)
			hp := func(b []byte) string { return fmt.Sprintf(`{"headers":[{"rlp_hex":"%s"}]}`, hex.EncodeToString(b)) }
			refused("F4", verifierID, "submitHeaders", hp(badEnc), "")
			if st, msg := attempt(verifierID, "submitHeaders", hp(enc)); st != "CONFIRMED" || vState("gapl")["gapl"] == gap["gapl"] {
				t.Errorf("F4 control FAIL: the real header %d was %s (%q), gap low %s", low-1, st, msg, vState("gapl")["gapl"])
			} else {
				t.Logf("F4 control PASS: the real header %d extends the backfill (gap low %s -> %s)", low-1, gap["gapl"], vState("gapl")["gapl"])
			}
		}
		// F8: restart with --max-proofs 0: backfill only.
		out, _, err := runOperator(t, ctx, filepath.Join(logDir, "operator-T2-restart.log"), opEnv, opArgsFor(verifierID, execRPC, 0), 40*time.Minute, nil)
		logOp("T2-restart", out)
		if err != nil || len(operatorLines(out, "received from the Prover Network")) != 0 {
			t.Errorf("F8 FAIL: restart with --max-proofs 0: exit %v, paid proofs %d", err, len(operatorLines(out, "received from the Prover Network")))
		} else {
			t.Logf("F8 PASS: restarted with --max-proofs 0, backfilled without buying a proof")
		}
	}
	checkProof("T2", before, lastT2)

	// F6: forged confirmSpends for d-0, before any bot confirms.
	csPayload := func(nonce uint64, s sent, blockDelta int64, amountDelta int64, toOverride string) (string, error) {
		idx := uint64(s.rc.TransactionIndex)
		_, txp, err := pr.TxTrieProof(ctx, s.block, idx)
		if err != nil {
			return "", err
		}
		_, rcp, err := pr.ReceiptTrieProof(ctx, s.block, idx)
		if err != nil {
			return "", err
		}
		txB64, err := b64Proof(txp)
		if err != nil {
			return "", err
		}
		rcB64, err := b64Proof(rcp)
		if err != nil {
			return "", err
		}
		n, to, amount, asset, err := pendingSpend(stored[nonce])
		if err != nil {
			return "", err
		}
		if toOverride != "" {
			to = toOverride
		}
		a, _ := new(big.Int).SetString(amount, 10)
		if a == nil {
			return "", fmt.Errorf("stored amount %q", amount)
		}
		a.Add(a, big.NewInt(amountDelta))
		return fmt.Sprintf(`{"block_height":%d,"tx_index":%d,"tx_proof":"%s","receipt_proof":"%s","intent_nonce":%d,"intent_to":"%s","intent_amount":"%s","intent_asset":"%s"}`,
			int64(s.block)+blockDelta, idx, txB64, rcB64, n, to, a, asset), nil
	}
	real0, err := csPayload(0, sE, 0, 0, "")
	if err != nil {
		t.Errorf("F6 NOT RUN: %v", err)
	} else {
		if other, err := csPayload(0, sE, 0, 0, cryptoPubkeyAddr(attKey).Hex()); err == nil {
			refused("F6-to", e.amID, "confirmSpend", other, "")
		}
		if raised, err := csPayload(0, sE, 0, 1, ""); err == nil {
			refused("F6-amount", e.amID, "confirmSpend", raised, "")
		}
		if wrongBlock, err := csPayload(0, sE, -1, 0, ""); err == nil {
			refused("F6-block", e.amID, "confirmSpend", wrongBlock, "")
		}
		if e.stateStr("n") != "0" || e.stateStr("d-0") == "" {
			t.Errorf("F6 FAIL: forged confirmSpends changed the queue: n=%q d-0=%v", e.stateStr("n"), e.stateStr("d-0") != "")
		}
	}

	// F10: a new bot from the first one's checkpoint, a dead RPC listed first.
	vaultNonce, _ := l1.cli.NonceAt(ctx, vault, nil)
	bot2 := StartBot(t, ctx, BotConfig{
		EthRPC: "http://127.0.0.1:9," + execRPC, ContractID: e.amID, GraphQLURL: d.GQLEndpoint(3),
		EthPrivHex: botHex, Checkpoint: cpPath, Tokens: usdcAddr.Hex(),
	})
	t.Cleanup(bot2.Stop)
	if err := pollUntil(ctx, 15*time.Minute, func() bool { return e.stateStr("n") == "1" && e.stateStr("d-0") == "" }); err != nil {
		t.Fatalf("confirmSpend never closed d-0: n=%q d-0=%v\nbot2:\n%s", e.stateStr("n"), e.stateStr("d-0") != "", botLogs(bot2))
	}
	if n, _ := l1.cli.NonceAt(ctx, vault, nil); n != vaultNonce || n != 1 {
		t.Errorf("F10 FAIL: the vault's L1 nonce went %d -> %d; the new bot sent again", vaultNonce, n)
	} else {
		t.Logf("F10 PASS: the new bot failed over past the dead RPC and confirmed d-0; vault nonce still %d", n)
	}
	t.Logf("(6a) PASS: confirmSpend closed the ETH withdrawal from proven headers (n=1)")
	if real0 != "" {
		refused("F6-replay", e.amID, "confirmSpend", real0, "")
	}

	// F15 / F16 credits, mapped by the new bot from T2's headers.
	f15 := new(big.Int).Mul(new(big.Int).Sub(smallDep, new(big.Int).Div(smallDep, big.NewInt(100))), big.NewInt(2))
	wantEth := new(big.Int)
	wantEth.SetString(afterE["eth"], 10)
	wantEth.Add(wantEth, f15)
	if err := pollUntil(ctx, 15*time.Minute, func() bool { return e.stateStr("a-"+user+"-eth") == wantEth.String() }); err != nil {
		t.Errorf("F15 FAIL: legacy + access-list deposits: user eth %s, want %s (each 0.001 less 1%%)", e.stateStr("a-"+user+"-eth"), wantEth)
	} else {
		t.Logf("F15 PASS: type 0 and type 1 deposits credited (user eth %s)", wantEth)
	}
	if fwdOK {
		t.Logf("F16 OBSERVED (E18): %s wei reached the vault through a contract's internal call (tx %s); credited to no one: user eth %s, reserve %s",
			smallDep, rcFwd.TxHash.Hex(), e.stateStr("a-"+user+"-eth"), e.stateStr("gr"))
	}

	// (5b) The USDC withdrawal (d-1), now that d-0 is confirmed.
	scanFrom, err = l1.cli.BlockNumber(ctx)
	if err != nil {
		t.Fatal(err)
	}
	qU := queue("unmapERC20", usdcUnmap, 1)
	sU := waitSent(1, qU, scanFrom, func() string { return botLogs(bot2) })
	if got, _ := l1.usdcBalance(ctx, recipU); got == nil || got.Cmp(big.NewInt(1_000_000)) != 0 {
		t.Fatalf("USDC recipient holds %v, want 1000000", got)
	}
	afterU := e.stateStr("a-"+user+"-eth") + "|" + e.stateStr("a-"+user+"-usdc")
	t.Logf("(5b) PASS: the TSS-signed USDC withdrawal paid 1 USDC on Sepolia (gas cap %s); reserve now %s", qU.GasFeeCap, e.stateStr("gr"))

	// (6b) T3: a third proof covers it; the bot confirms it.
	if err := l1.waitFinalized(ctx, sU.block, 40*time.Minute); err != nil {
		t.Fatal(err)
	}
	before = vState("bh", "h")
	out3, _, err := runOperator(t, ctx, filepath.Join(logDir, "operator-T3.log"), opEnv, opArgsFor(verifierID, execRPC, 1), 100*time.Minute, nil)
	logOp("T3", out3)
	if err != nil || len(operatorLines(out3, "proof 1 received from the Prover Network")) != 1 || len(operatorLines(out3, "1 proof(s) landed")) == 0 {
		t.Fatalf("T3: operator exit %v; the log does not show exactly one paid proof that landed", err)
	}
	checkProof("T3", before, sU.block)
	if err := pollUntil(ctx, 15*time.Minute, func() bool { return e.stateStr("n") == "2" && e.stateStr("d-1") == "" }); err != nil {
		t.Fatalf("confirmSpend never closed d-1: n=%q d-1=%v\nbot2:\n%s", e.stateStr("n"), e.stateStr("d-1") != "", botLogs(bot2))
	}
	if n, _ := l1.cli.NonceAt(ctx, vault, nil); n != 2 {
		t.Errorf("vault L1 nonce %d after two withdrawals, want 2", n)
	}
	if got := e.stateStr("a-"+user+"-eth") + "|" + e.stateStr("a-"+user+"-usdc"); got != afterU {
		t.Fatalf("confirmSpend moved the user's balances: %s -> %s", afterU, got)
	}
	t.Logf("(6b) PASS: confirmSpend closed the USDC withdrawal (n=2)")

	// RP-4 on real data: the USDC gas left the reserve below the floor, so a
	// funded user's ETH withdrawal is refused. Inverts once RP-4 is fixed.
	if st, msg := userCall("unmapETH", ethUnmap); st != "FAILED" || !strings.Contains(msg, "insufficient gas reserve") || e.stateStr("d-2") != "" {
		t.Errorf("RP-4 observation: an ETH withdrawal with the reserve at %s was %s (%q), d-2 queued=%v", e.stateStr("gr"), st, msg, e.stateStr("d-2") != "")
	} else {
		t.Logf("RP-4 OBSERVED on real data: reserve %s (< 0.05 floor); the user's funded 0.01 ETH withdrawal refused: %s", e.stateStr("gr"), msg)
	}
	lastL1 := lastT2
	if sU.block > lastL1 {
		lastL1 = sU.block
	}

	// F12: node 4 caught up.
	if err := pollUntil(ctx, 10*time.Minute, func() bool {
		m, _ := d.GetStateByKeys(ctx, 4, e.amID, []string{"n"})
		return m != nil && fmt.Sprint(m["n"]) == "2"
	}); err != nil {
		t.Errorf("F12 FAIL: node 4 never caught up after its restart")
	} else {
		t.Logf("F12 PASS: node 4 rejoined and serves n=2")
	}

	// (7) Replay: a bot with a fresh checkpoint rescans everything.
	final := balances()
	bot2.Stop()
	cp3 := filepath.Join(logDir, "bot3-checkpoint.json")
	seedCheckpoint(cp3)
	bot3 := StartBot(t, ctx, BotConfig{
		EthRPC: execRPC, ContractID: e.amID, GraphQLURL: d.GQLEndpoint(3),
		EthPrivHex: botHex, Checkpoint: cp3, Tokens: usdcAddr.Hex(),
	})
	t.Cleanup(bot3.Stop)
	if err := pollUntil(ctx, 25*time.Minute, func() bool {
		raw, err := os.ReadFile(cp3)
		var c struct {
			Last uint64 `json:"last_scanned_block"`
		}
		return err == nil && json.Unmarshal(raw, &c) == nil && c.Last >= lastL1
	}); err != nil {
		t.Fatalf("the rescanning bot never got past block %d\nbot3:\n%s", lastL1, botLogs(bot3))
	}
	devnetSleep(2 * time.Minute) // let any map it submitted land
	if got := balances(); got != final {
		t.Fatalf("a rescan re-credited a deposit: %s -> %s", final, got)
	}
	t.Logf("(7) PASS: a full rescan of the real deposits credited nothing twice (%s)", final)

	// F14 (RP-5): a bot handed a mainnet RPC for this Sepolia bridge.
	bot3.Stop()
	cp4 := filepath.Join(logDir, "bot4-checkpoint.json")
	seedCheckpoint(cp4)
	bot4 := StartBot(t, ctx, BotConfig{
		EthRPC: mainnetRPC, ContractID: e.amID, GraphQLURL: d.GQLEndpoint(3),
		EthPrivHex: botHex, Checkpoint: cp4, Tokens: usdcAddr.Hex(),
	})
	t.Cleanup(bot4.Stop)
	devnetSleep(90 * time.Second)
	refusedChain := strings.Contains(strings.ToLower(botLogs(bot4)), "chain")
	if got := balances(); got != final {
		t.Errorf("F14 FAIL: a bot on the wrong chain moved balances: %s -> %s", final, got)
	}
	t.Logf("F14 OBSERVED (RP-5): a bot on a mainnet RPC for a Sepolia bridge; mentions the chain in its logs: %v; balances unchanged", refusedChain)

	checkVaultKey("end of run")
	// M3: RPC trouble the bots met on the public endpoint.
	for name, b := range map[string]*Bot{"bot": bot, "botB": botB, "bot2": bot2, "bot3": bot3} {
		l := strings.ToLower(botLogs(b))
		t.Logf("M3 %s: %d log lines; '429' %d, 'rate limit' %d, 'scan block failed' %d, 'timeout' %d",
			name, len(b.Logs()), strings.Count(l, "429"), strings.Count(l, "rate limit"), strings.Count(l, "scan block failed"), strings.Count(l, "timeout"))
	}
}
