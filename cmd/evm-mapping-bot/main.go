package main

// EVM Mapping Bot — standalone runner for Ethereum deposit scanning,
// withdrawal TX assembly, broadcast, and confirmSpend submission.
//
// Uses go-vsc-node's transaction-pool and dids packages for L2 submission.
// Signs transactions with an secp256k1 key (BOT_ETH_PRIVKEY env var).
//
// Usage:
//   evm-mapping-bot  (configure via env vars)

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"vsc-node/lib/dids"
	"vsc-node/modules/db/vsc/contracts"
	transactionpool "vsc-node/modules/transaction-pool"

	ethCrypto "github.com/ethereum/go-ethereum/crypto"
	"golang.org/x/crypto/sha3"
)

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

type EVMBotConfig struct {
	EthRPC         string
	VaultAddress   string            // 0x... lowercase
	Tokens         map[string]string // address → symbol (lowercase keys)
	ContractID     string
	GraphQLURLs    []string
	PollInterval   time.Duration
	Network        string
	CheckpointFile string
	NetID          string // vsc-mainnet or vsc-testnet
	RcLimit        uint64
}

// l2Submitter handles L2 transaction signing and submission.
type l2Submitter struct {
	ethKey *ecdsa.PrivateKey
	did    dids.EthDID
	gql    *vscGraphQL
	cfg    EVMBotConfig
	mu     sync.Mutex // serializes L2 submissions for nonce ordering
}

func main() {
	cfg := parseConfig()

	// Initialize L2 signing key
	ethKey, did := initEthKey()

	slog.Info("evm-mapping-bot starting",
		"rpc", cfg.EthRPC,
		"vault", cfg.VaultAddress,
		"contract", cfg.ContractID,
		"tokens", len(cfg.Tokens),
		"network", cfg.Network,
		"netId", cfg.NetID,
		"did", did.String(),
	)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := RunLoop(ctx, cfg, ethKey, did); err != nil && err != context.Canceled {
		slog.Error("bot exited with error", "err", err)
		os.Exit(1)
	}
	slog.Info("bot shut down cleanly")
}

func parseConfig() EVMBotConfig {
	rcLimit := uint64(10000)
	if v := os.Getenv("RC_LIMIT"); v != "" {
		if parsed, err := strconv.ParseUint(v, 10, 64); err == nil {
			rcLimit = parsed
		}
	}

	cfg := EVMBotConfig{
		EthRPC:         envOrDefault("ETH_RPC", "http://localhost:8545"),
		VaultAddress:   strings.ToLower(envOrDefault("VAULT_ADDRESS", "")),
		ContractID:     envOrDefault("CONTRACT_ID", ""),
		PollInterval:   12 * time.Second,
		Network:        envOrDefault("NETWORK", "mainnet"),
		Tokens:         map[string]string{},
		GraphQLURLs:    strings.Split(envOrDefault("GRAPHQL_URLS", "https://api.vsc.eco/api/v1/graphql"), ","),
		CheckpointFile: envOrDefault("CHECKPOINT_FILE", "evm-bot-checkpoint.json"),
		NetID:          envOrDefault("NET_ID", "vsc-mainnet"),
		RcLimit:        rcLimit,
	}

	if cfg.Network == "mainnet" {
		cfg.Tokens["0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48"] = "usdc"
	}

	if cfg.VaultAddress == "" || cfg.ContractID == "" {
		slog.Error("VAULT_ADDRESS and CONTRACT_ID must be set")
		os.Exit(1)
	}

	// MED-44 (EVM-B-M4): validate the vault address FORMAT, not just non-empty.
	// A zero address (0x000...0) or any non-20-byte / non-hex string previously
	// passed the "!= \"\"" check, after which the bot would treat burns to
	// address(0) as deposits and sign withdrawals the contract can never confirm.
	if !isValidEthAddress(cfg.VaultAddress) {
		slog.Error("VAULT_ADDRESS is not a valid 20-byte hex Ethereum address (or is the zero address)",
			"vault", cfg.VaultAddress)
		os.Exit(1)
	}

	return cfg
}

// isValidEthAddress reports whether s is a 0x-prefixed 20-byte hex address that
// is not the zero address. Used to reject vault misconfiguration at startup.
func isValidEthAddress(s string) bool {
	if !strings.HasPrefix(s, "0x") && !strings.HasPrefix(s, "0X") {
		return false
	}
	body := s[2:]
	if len(body) != 40 {
		return false
	}
	raw, err := hex.DecodeString(body)
	if err != nil {
		return false
	}
	allZero := true
	for _, b := range raw {
		if b != 0 {
			allZero = false
			break
		}
	}
	return !allZero
}

// initEthKey loads or generates the secp256k1 private key for L2 signing.
func initEthKey() (*ecdsa.PrivateKey, dids.EthDID) {
	keyHex := os.Getenv("BOT_ETH_PRIVKEY")
	if keyHex != "" {
		priv, err := ethCrypto.HexToECDSA(keyHex)
		if err != nil {
			slog.Error("invalid BOT_ETH_PRIVKEY", "err", err)
			os.Exit(1)
		}
		addr := ethCrypto.PubkeyToAddress(priv.PublicKey).Hex()
		did := dids.NewEthDID(addr)
		slog.Info("loaded L2 signing key from BOT_ETH_PRIVKEY", "did", did.String())
		return priv, did
	}

	// Auto-generate
	priv, err := ethCrypto.GenerateKey()
	if err != nil {
		slog.Error("failed to generate signing key", "err", err)
		os.Exit(1)
	}
	addr := ethCrypto.PubkeyToAddress(priv.PublicKey).Hex()
	did := dids.NewEthDID(addr)
	privHex := hex.EncodeToString(ethCrypto.FromECDSA(priv))
	slog.Warn("generated new L2 signing key — fund this DID with HBD before the bot can submit transactions",
		"did", did.String(),
		"privkey_hex", privHex,
	)
	slog.Warn("set BOT_ETH_PRIVKEY to persist this key across restarts")
	return priv, did
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// ---------------------------------------------------------------------------
// Checkpoint — persists last scanned block + sent withdrawal TXs to disk.
// On restart, the bot resumes from its last checkpoint. Deposits already
// submitted are idempotent (contract's o-{height} observed list deduplicates).
// ---------------------------------------------------------------------------

type Checkpoint struct {
	LastScannedBlock uint64            `json:"last_scanned_block"`
	SentWithdrawals  map[string]SentTx `json:"sent_withdrawals"`
	BlockRetries     map[uint64]int    `json:"block_retries,omitempty"`
	Version          int               `json:"version"`
	mu               sync.Mutex
}

const blockRetryAlertThreshold = 10

// checkpointVersion is the on-disk schema version. Bumping the struct layout
// MUST bump this so a future bot refuses to silently consume a stale layout.
const checkpointVersion = 1

type SentTx struct {
	SignedTxHex string `json:"signed_tx_hex"`
	TxHash      string `json:"tx_hash"`
	Nonce       uint64 `json:"nonce"`
	SentAt      int64  `json:"sent_at"`
}

func loadCheckpoint(path string) *Checkpoint {
	cp := &Checkpoint{
		SentWithdrawals: make(map[string]SentTx),
		BlockRetries:    make(map[uint64]int),
		Version:         checkpointVersion,
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cp
	}
	// MED-43 / MED-137 (EVM-B-M2/M8, m59 F11): do NOT discard the unmarshal
	// error. A corrupt/partial checkpoint previously yielded Checkpoint{} —
	// the bot then re-scanned from block 1 and could rebroadcast already-sent
	// withdrawals at reused L1 nonces. On a decode error, fail closed: refuse
	// to start from a phantom-empty state rather than silently re-scanning.
	if err := json.Unmarshal(data, cp); err != nil {
		slog.Error("checkpoint file is corrupt — refusing to start from an empty state to avoid re-scan / nonce-reuse; move or repair the file",
			"path", path, "err", err)
		os.Exit(1)
	}
	// MED-43: reject an unknown on-disk schema version rather than mis-reading
	// fields written by an incompatible build.
	if cp.Version != 0 && cp.Version != checkpointVersion {
		slog.Error("checkpoint schema version mismatch — refusing to start",
			"path", path, "fileVersion", cp.Version, "expected", checkpointVersion)
		os.Exit(1)
	}
	cp.Version = checkpointVersion
	if cp.SentWithdrawals == nil {
		cp.SentWithdrawals = make(map[string]SentTx)
	}
	if cp.BlockRetries == nil {
		cp.BlockRetries = make(map[uint64]int)
	}
	return cp
}

func (cp *Checkpoint) save(path string) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	cp.Version = checkpointVersion
	data, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		slog.Error("checkpoint marshal failed", "err", err)
		return
	}
	tmpPath := path + ".tmp"
	// MED-120 / MED-90 (SSH-9 / OP-2): the checkpoint holds SignedTxHex and the
	// last-scanned block — broadcast-replay + OPSEC surface. Write 0600 so it is
	// not world-readable on shared hosts.
	if err := os.WriteFile(tmpPath, data, 0600); err != nil {
		slog.Error("checkpoint write failed", "path", tmpPath, "err", err)
		return
	}
	if err := os.Rename(tmpPath, path); err != nil {
		slog.Error("checkpoint rename failed", "err", err)
	}
}

// acquireCheckpointLock takes an exclusive advisory lock on a sidecar lock file
// next to the checkpoint. MED-91 (M32-NEW-9): two bot instances pointed at the
// same checkpoint would otherwise race competing broadcasts at the same L2/L1
// nonce. A non-blocking flock makes a second instance fail fast instead of
// silently double-broadcasting. The returned *os.File must be kept open for the
// lifetime of the process (closing releases the lock).
func acquireCheckpointLock(path string) (*os.File, error) {
	lockPath := path + ".lock"
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", lockPath, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another bot instance already holds %s: %w", lockPath, err)
	}
	return f, nil
}

// ---------------------------------------------------------------------------
// Ethereum RPC client — thin wrapper for JSON-RPC calls.
// ---------------------------------------------------------------------------

type ethRPC struct {
	url    string
	client *http.Client
}

func newEthRPC(url string) *ethRPC {
	return &ethRPC{url: url, client: &http.Client{Timeout: 30 * time.Second}}
}

// maxHTTPResponseBytes caps how many bytes we will read from any single HTTP
// response body (JSON-RPC or GraphQL). MED-57 (EVM-B-M5 / F-ST-2 / F-MON-3 /
// EH-MED-5): a hostile or buggy upstream RPC/GQL endpoint could otherwise stream
// an unbounded body into io.ReadAll and OOM-kill the bot, halting the bridge. A
// legitimate eth_getBlockByNumber with full txs for a ~300-tx ETH block is well
// under this; 16 MiB leaves generous headroom for L2 forks with denser blocks
// while still bounding worst-case memory.
const maxHTTPResponseBytes = 16 << 20 // 16 MiB

// gqlPerURLTimeout bounds how long any single GraphQL endpoint may take before
// the bot abandons it and rotates to the next URL. MED-116 (m39 F-INJ-B4): the
// GraphQL clients walk all configured URLs sequentially; without a per-URL cap a
// slow-loris endpoint (or several) could hold each request for the full client
// Timeout, so N URLs serialized into N*30s and stalled the withdrawal pipeline.
// A tight per-URL deadline makes a hung endpoint fail fast and bounds the total
// walk to len(urls)*gqlPerURLTimeout (further clamped by the parent context).
const gqlPerURLTimeout = 12 * time.Second

// readBodyCapped reads up to maxHTTPResponseBytes from r and returns an error if
// the body exceeds the cap (rather than silently truncating, which would corrupt
// JSON decoding). MED-57: mirrors the io.LimitReader(body, max+1) bounded-read
// pattern already used at modules/wasm/runtime_ipc/setup_unix.go:89.
func readBodyCapped(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxHTTPResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxHTTPResponseBytes {
		return nil, fmt.Errorf("response body exceeds %d byte cap", maxHTTPResponseBytes)
	}
	return data, nil
}

func (e *ethRPC) call(method string, params string) (json.RawMessage, error) {
	body := fmt.Sprintf(`{"jsonrpc":"2.0","method":"%s","params":[%s],"id":1}`, method, params)
	resp, err := e.client.Post(e.url, "application/json", strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// MED-57: bound the response body so a hostile RPC cannot OOM the bot.
	raw, err := readBodyCapped(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read rpc response: %w", err)
	}

	var result struct {
		Result json.RawMessage           `json:"result"`
		Error  *struct{ Message string } `json:"error"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("malformed rpc response: %w", err)
	}
	if result.Error != nil {
		return nil, fmt.Errorf("rpc error: %s", result.Error.Message)
	}
	if result.Result == nil || string(result.Result) == "null" {
		return nil, fmt.Errorf("null result from %s", method)
	}
	return result.Result, nil
}

func (e *ethRPC) getFinalizedBlock() (uint64, error) {
	data, err := e.call("eth_getBlockByNumber", `"finalized", false`)
	if err != nil {
		return 0, err
	}
	var block struct {
		Number string `json:"number"`
	}
	json.Unmarshal(data, &block)
	return hexToUint64(block.Number), nil
}

func (e *ethRPC) getBlockWithTxs(height uint64) (json.RawMessage, error) {
	return e.call("eth_getBlockByNumber", fmt.Sprintf(`"0x%x", true`, height))
}

func (e *ethRPC) getReceipt(txHash string) (json.RawMessage, error) {
	return e.call("eth_getTransactionReceipt", fmt.Sprintf(`"%s"`, txHash))
}

// getTransactionByHash fetches the canonical transaction object for a hash.
// MED-50: used to cross-check that a receipt returned by "already mined"
// recovery actually corresponds to a transaction the RPC knows about, so a
// hostile RPC cannot fabricate a receipt for a hash we never broadcast.
func (e *ethRPC) getTransactionByHash(txHash string) (json.RawMessage, error) {
	return e.call("eth_getTransactionByHash", fmt.Sprintf(`"%s"`, txHash))
}

func (e *ethRPC) broadcastTx(signedTxHex string) (string, error) {
	data, err := e.call("eth_sendRawTransaction", fmt.Sprintf(`"0x%s"`, signedTxHex))
	if err != nil {
		return "", err
	}
	var txHash string
	json.Unmarshal(data, &txHash)
	return txHash, nil
}

// fetchContractPaused reports whether the contract's "paused" state key is "1".
// MED-51 (F-M37-7): lets the tick loop detect a paused contract and back off
// instead of burning RC retrying calls that the contract will reject.
func fetchContractPaused(ctx context.Context, gql *vscGraphQL, contractID string) (bool, error) {
	state, err := gql.fetchContractState(ctx, contractID, []string{"paused"})
	if err != nil {
		return false, err
	}
	return state["paused"] == "1", nil
}

// ---------------------------------------------------------------------------
// VSC GraphQL client — queries contract state and submits L2 transactions.
// ---------------------------------------------------------------------------

type vscGraphQL struct {
	urls   []string
	client *http.Client
}

func newVSCGraphQL(urls []string) *vscGraphQL {
	return &vscGraphQL{urls: urls, client: &http.Client{Timeout: 30 * time.Second}}
}

func (g *vscGraphQL) query(ctx context.Context, gqlQuery string, variables map[string]interface{}) (json.RawMessage, error) {
	body, _ := json.Marshal(map[string]interface{}{
		"query":     gqlQuery,
		"variables": variables,
	})

	var lastErr error
	for i, url := range g.urls {
		// MED-115 (m39 F-INJ-B3): rotating to a fallback endpoint after a primary
		// failure must NOT be silent. A transient 502 on URL[0] followed by a 200
		// from a less-trusted fallback previously looked identical to a clean
		// success — there was no signal the bot had degraded to a secondary
		// source (and may be acting on its possibly-staler data). We still fail
		// CLOSED on total exhaustion (return error below); here we make every
		// fall-through observable so a stuck-on-fallback condition is detectable.
		res, err := g.doGQL(ctx, url, body)
		if err != nil {
			lastErr = err
			if i < len(g.urls)-1 {
				slog.Warn("MED-115: GraphQL endpoint failed — rotating to next endpoint (degraded to fallback; data may be staler)",
					"failedURL", url, "err", err)
			}
			continue
		}

		var result struct {
			Data   json.RawMessage `json:"data"`
			Errors []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		if err := json.Unmarshal(res, &result); err != nil {
			lastErr = fmt.Errorf("decode: %w", err)
			if i < len(g.urls)-1 {
				slog.Warn("MED-115: GraphQL endpoint returned undecodable body — rotating to next endpoint",
					"failedURL", url, "err", err)
			}
			continue
		}
		if len(result.Errors) > 0 {
			// A GraphQL-level error is an authoritative answer from the endpoint,
			// not a transport failure — do NOT silently rotate past it (that would
			// be the fail-open the audit flags). Fail closed.
			return nil, fmt.Errorf("graphql error: %s", result.Errors[0].Message)
		}
		return result.Data, nil
	}
	return nil, fmt.Errorf("all graphql endpoints failed: %w", lastErr)
}

// doGQL performs one POST to a single GraphQL endpoint with a per-URL deadline,
// reads the response under a size cap, and validates the HTTP status. It is the
// shared per-endpoint primitive for the URL-rotation loops.
//   - MED-116 (m39 F-INJ-B4): each attempt gets its own bounded sub-context
//     (gqlPerURLTimeout, further clamped by the parent ctx) so one slow-loris
//     endpoint cannot consume the whole client Timeout and serialize N URLs into
//     N*30s — a hung endpoint fails fast and the walk rotates on.
//   - MED-57 (EVM-B-M5): the body is read through readBodyCapped so a hostile
//     endpoint cannot OOM the bot with an unbounded response.
func (g *vscGraphQL) doGQL(ctx context.Context, url string, body []byte) (json.RawMessage, error) {
	reqCtx, cancel := context.WithTimeout(ctx, gqlPerURLTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := readBodyCapped(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response from %s: %w", url, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	return raw, nil
}

// fetchContractState reads keys from the EVM mapping contract's state.
func (g *vscGraphQL) fetchContractState(ctx context.Context, contractID string, keys []string) (map[string]string, error) {
	data, err := g.query(ctx,
		`query GetState($contractId: String!, $keys: [String!]!, $encoding: String) {
			getStateByKeys(contractId: $contractId, keys: $keys, encoding: $encoding)
		}`,
		map[string]interface{}{
			"contractId": contractID,
			"keys":       keys,
			"encoding":   "raw",
		},
	)
	if err != nil {
		return nil, err
	}

	var parsed struct {
		GetStateByKeys json.RawMessage `json:"getStateByKeys"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("decode state: %w", err)
	}

	result := make(map[string]string)

	var obj map[string]string
	if json.Unmarshal(parsed.GetStateByKeys, &obj) == nil {
		return obj, nil
	}

	var arr []string
	if json.Unmarshal(parsed.GetStateByKeys, &arr) == nil {
		for i, v := range arr {
			if i < len(keys) {
				result[keys[i]] = v
			}
		}
		return result, nil
	}

	return result, nil
}

// fetchTssSignatures queries getTssRequests for completed signatures.
func (g *vscGraphQL) fetchTssSignatures(ctx context.Context, keyID string, msgHexList []string) (map[string]TssSignature, error) {
	data, err := g.query(ctx,
		`query GetTssRequests($keyId: String!, $msgHex: [String!]!) {
			getTssRequests(keyId: $keyId, msgHex: $msgHex) {
				msg
				sig
				status
			}
		}`,
		map[string]interface{}{
			"keyId":  keyID,
			"msgHex": msgHexList,
		},
	)
	if err != nil {
		return nil, err
	}

	var parsed struct {
		GetTssRequests []struct {
			Msg    string `json:"msg"`
			Sig    string `json:"sig"`
			Status string `json:"status"`
		} `json:"getTssRequests"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("decode tss: %w", err)
	}

	out := make(map[string]TssSignature)
	for _, r := range parsed.GetTssRequests {
		if r.Status != "complete" {
			continue
		}
		sigBytes, err := hex.DecodeString(r.Sig)
		if err != nil {
			slog.Warn("invalid signature hex from TSS", "msg", r.Msg, "err", err)
			continue
		}
		out[r.Msg] = TssSignature{Bytes: sigBytes}
	}
	return out, nil
}

type TssSignature struct {
	Bytes []byte
}

// FetchAccountNonce queries the VSC node for the next unused nonce of a given
// account (did:pkh:eip155:1:0x...). Used as the nonce header on L2 txs.
func (g *vscGraphQL) FetchAccountNonce(ctx context.Context, account string) (uint64, error) {
	reqBody, _ := json.Marshal(map[string]any{
		"query":     `query($a: String!){ getAccountNonce(account: $a){ nonce } }`,
		"variables": map[string]any{"a": account},
	})

	var lastErr error
	for i, url := range g.urls {
		// MED-57 + MED-116: capped read + per-URL deadline via doGQL.
		raw, err := g.doGQL(ctx, url, reqBody)
		if err != nil {
			lastErr = err
			if i < len(g.urls)-1 {
				slog.Warn("MED-115: nonce endpoint failed — rotating to next endpoint",
					"failedURL", url, "err", err)
			}
			continue
		}

		var result struct {
			Data struct {
				GetAccountNonce struct {
					Nonce uint64 `json:"nonce"`
				} `json:"getAccountNonce"`
			} `json:"data"`
			Errors []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			lastErr = fmt.Errorf("decode nonce: %w", err)
			if i < len(g.urls)-1 {
				slog.Warn("MED-115: nonce endpoint returned undecodable body — rotating to next endpoint",
					"failedURL", url, "err", err)
			}
			continue
		}
		if len(result.Errors) > 0 {
			return 0, fmt.Errorf("graphql error: %s", result.Errors[0].Message)
		}
		return result.Data.GetAccountNonce.Nonce, nil
	}
	return 0, fmt.Errorf("all graphql endpoints failed (FetchAccountNonce): %w", lastErr)
}

// SubmitTransactionV1 submits a signed VSC L2 transaction via the node's
// submitTransactionV1 mutation and returns the resulting CID tx ID.
func (g *vscGraphQL) SubmitTransactionV1(ctx context.Context, txB64, sigB64 string) (string, error) {
	reqBody, _ := json.Marshal(map[string]any{
		"query":     `query($tx: String!, $sig: String!){ submitTransactionV1(tx: $tx, sig: $sig){ id } }`,
		"variables": map[string]any{"tx": txB64, "sig": sigB64},
	})

	var lastErr error
	for i, url := range g.urls {
		// MED-57 + MED-116: capped read + per-URL deadline via doGQL. Rotation
		// semantics are unchanged from the original loop; L2 nonce-reuse / double-
		// submit safety on this broadcast path is enforced upstream in
		// callContractL2/callWithRetry (errNonceConsumed, context-done abort).
		raw, err := g.doGQL(ctx, url, reqBody)
		if err != nil {
			lastErr = err
			if i < len(g.urls)-1 {
				slog.Warn("MED-115: submit endpoint failed — rotating to next endpoint",
					"failedURL", url, "err", err)
			}
			continue
		}

		var result struct {
			Data struct {
				SubmitTransactionV1 struct {
					ID *string `json:"id"`
				} `json:"submitTransactionV1"`
			} `json:"data"`
			Errors []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			lastErr = fmt.Errorf("decode submit: %w", err)
			if i < len(g.urls)-1 {
				slog.Warn("MED-115: submit endpoint returned undecodable body — rotating to next endpoint",
					"failedURL", url, "err", err)
			}
			continue
		}
		if len(result.Errors) > 0 {
			return "", fmt.Errorf("graphql error: %s", result.Errors[0].Message)
		}
		if result.Data.SubmitTransactionV1.ID == nil {
			return "", fmt.Errorf("submitTransactionV1 returned nil id")
		}
		return *result.Data.SubmitTransactionV1.ID, nil
	}
	return "", fmt.Errorf("all graphql endpoints failed (SubmitTransactionV1): %w", lastErr)
}

// ---------------------------------------------------------------------------
// L2 contract call — mirrors mapper.Bot.callContractL2
// ---------------------------------------------------------------------------

// callContractL2 submits a vsc.call contract invocation through the VSC L2
// transaction pool using the bot's did:pkh:eip155 identity.
func (s *l2Submitter) callContractL2(
	ctx context.Context,
	contractID string,
	action string,
	payload json.RawMessage,
	rcOverride uint64,
) (string, error) {
	// Serialize concurrent L2 submissions for nonce ordering.
	s.mu.Lock()
	defer s.mu.Unlock()

	did := s.did

	nonce, err := s.gql.FetchAccountNonce(ctx, did.String())
	if err != nil {
		return "", fmt.Errorf("fetch L2 nonce: %w", err)
	}

	rcLimit := s.cfg.RcLimit
	// MED-99 (M33-M6): allow per-call RcLimit escalation so a large-instruction
	// (gas-bomb) deposit that exceeds the default RC budget can be recovered by
	// retrying with a higher limit instead of being permanently dead-lettered.
	if rcOverride > 0 {
		rcLimit = rcOverride
	}
	call := &transactionpool.VscContractCall{
		ContractId: contractID,
		Action:     action,
		Payload:    string(payload),
		RcLimit:    rcLimit,
		Intents:    []contracts.Intent{},
		Caller:     did.String(),
		NetId:      s.cfg.NetID,
	}
	op, err := call.SerializeVSC()
	if err != nil {
		return "", fmt.Errorf("serialize L2 op: %w", err)
	}

	vscTx := transactionpool.VSCTransaction{
		Ops:     []transactionpool.VSCTransactionOp{op},
		Nonce:   nonce,
		NetId:   s.cfg.NetID,
		RcLimit: rcLimit,
	}

	crafter := transactionpool.TransactionCrafter{
		Identity: dids.NewEthProvider(s.ethKey),
		Did:      did,
	}
	sTx, err := crafter.SignFinal(vscTx)
	if err != nil {
		return "", fmt.Errorf("sign L2 tx: %w", err)
	}

	if len(sTx.Tx) > transactionpool.MAX_TX_SIZE {
		slog.Error("L2 transaction exceeds maximum size",
			"action", action,
			"cbor_size", len(sTx.Tx),
			"limit", transactionpool.MAX_TX_SIZE,
		)
		return "", fmt.Errorf("L2 tx too large: %d bytes (limit %d)", len(sTx.Tx), transactionpool.MAX_TX_SIZE)
	}

	txID, err := s.gql.SubmitTransactionV1(
		ctx,
		base64.URLEncoding.EncodeToString(sTx.Tx),
		base64.URLEncoding.EncodeToString(sTx.Sig),
	)
	if err != nil {
		// MED-98 (M33-M4): the bot uses a single freshly-fetched L2 account
		// nonce per submission. If a prior attempt actually landed (but the bot
		// observed an error), this attempt reuses a now-consumed nonce and the
		// node rejects it as "nonce too low" / "already known" / "duplicate".
		// Re-fetching the same stale nonce and retrying forever would stall ALL
		// subsequent submissions. Treat an already-consumed-nonce rejection as
		// terminal-progress (errNonceConsumed) so the retry layer stops cleanly
		// instead of wedging — the next tick re-derives a fresh nonce.
		if isNonceConsumedErr(err) {
			slog.Warn("MED-98: L2 nonce already consumed (prior submission likely landed) — not stalling on a stale-nonce retry",
				"action", action, "nonce", nonce, "err", err)
			return "", fmt.Errorf("%w: %v", errNonceConsumed, err)
		}
		return "", fmt.Errorf("broadcast L2 tx: %w", err)
	}

	slog.Info("L2 tx broadcast",
		"id", txID,
		"action", action,
		"nonce", nonce,
		"cbor_size", len(sTx.Tx),
		"did", did.String(),
	)
	return txID, nil
}

// callWithRetry submits an L2 contract call with retry logic.
// Retries up to maxAttempts times on broadcast failure with exponential backoff.
func (s *l2Submitter) callWithRetry(
	ctx context.Context,
	contractID string,
	action string,
	payload json.RawMessage,
	maxAttempts int,
) error {
	var lastErr error
	var rcOverride uint64 // 0 = use configured default
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		_, err := s.callContractL2(ctx, contractID, action, payload, rcOverride)
		if err == nil {
			return nil
		}
		// MED-98 (M33-M4): if the prior submission already consumed the nonce
		// (it landed), do NOT keep retrying at a stale nonce — that would stall
		// every later submission. Stop cleanly; the next tick re-derives a fresh
		// nonce and the contract dedupes the already-landed action.
		if errors.Is(err, errNonceConsumed) {
			slog.Info("L2 submission nonce already consumed — prior attempt landed; not retrying",
				"action", action)
			return nil
		}
		lastErr = err
		slog.Warn("L2 submission failed",
			"action", action,
			"attempt", attempt,
			"maxAttempts", maxAttempts,
			"err", err,
		)
		// MED-49 (F-M22-MED-5): if the surrounding context has been cancelled or
		// has timed out, the previous SubmitTransactionV1 may have reached the
		// node even though we observed an error. Retrying would re-fetch a fresh
		// nonce and re-submit the SAME action, double-spending RC (and, for
		// withdrawals, risking a second L1 broadcast). Stop retrying on an
		// ambiguous context-done outcome rather than risk a double-submit.
		if ctx.Err() != nil {
			return fmt.Errorf("L2 submission aborted (context done; prior attempt outcome ambiguous, not retrying to avoid double-submit): %w", lastErr)
		}
		// MED-99 (M33-M6) + MED-96 (M33-H1): on an RC-exhaustion error (a
		// gas-bomb payload) OR an RC-contention rejection (a competing bot
		// outbidding the honest bot on L2 mempool RC), escalate the RcLimit for
		// the next attempt (bounded). This lets a gas-bomb-sized payload be
		// recovered and lets the honest bot out-bid contention rather than be
		// censored. Full anti-censorship is a protocol-level priority concern
		// (structural) — this is the bot-local mitigation.
		if isRcExhaustionErr(err) || isRcContentionErr(err) {
			next := s.cfg.RcLimit * 4
			if rcOverride > 0 {
				next = rcOverride * 4
			}
			const maxRcEscalation = 1 << 22 // hard ceiling so escalation is bounded
			if next > maxRcEscalation {
				next = maxRcEscalation
			}
			rcOverride = next
			slog.Warn("MED-99/MED-96: RC exhausted or contended — escalating RcLimit for next attempt",
				"action", action, "newRcLimit", rcOverride)
		}
		if attempt < maxAttempts {
			backoff := time.Duration(attempt) * 2 * time.Second
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return fmt.Errorf("all %d L2 submission attempts failed: %w", maxAttempts, lastErr)
}

// errNonceConsumed is a sentinel: the L2 account nonce used for a submission was
// already consumed by a prior attempt that actually landed. MED-98 — callers
// must treat this as terminal-progress, NOT a retryable failure.
var errNonceConsumed = errors.New("l2 nonce already consumed")

// isNonceConsumedErr reports whether an L2 submission error indicates the nonce
// was already used (the prior attempt landed). MED-98.
func isNonceConsumedErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "nonce too low") ||
		strings.Contains(msg, "nonce already") ||
		strings.Contains(msg, "already known") ||
		strings.Contains(msg, "duplicate") ||
		strings.Contains(msg, "already in pool") ||
		strings.Contains(msg, "already exists")
}

// isRcExhaustionErr reports whether an L2 submission error looks like an RC /
// resource-credit exhaustion rejection. Used by MED-99 RcLimit escalation.
func isRcExhaustionErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "rc") &&
		(strings.Contains(msg, "exceed") ||
			strings.Contains(msg, "exhaust") ||
			strings.Contains(msg, "insufficient") ||
			strings.Contains(msg, "limit") ||
			strings.Contains(msg, "not enough"))
}

// isRcContentionErr reports whether an L2 submission error looks like an
// RC-contention / mempool-priority rejection (a competing bot outbidding the
// honest bot). MED-96 (M33-H1) — bot-local mitigation only; full anti-censorship
// is a protocol-level priority concern.
func isRcContentionErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "mempool") ||
		strings.Contains(msg, "replacement") ||
		strings.Contains(msg, "underpriced") ||
		strings.Contains(msg, "priority") ||
		strings.Contains(msg, "too many") ||
		strings.Contains(msg, "congest")
}

// ---------------------------------------------------------------------------
// Deposit scanning
// ---------------------------------------------------------------------------

// TransferEventSig is keccak256("Transfer(address,address,uint256)")
const TransferEventSig = "ddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"

type detectedDeposit struct {
	BlockHeight  uint64
	TxIndex      int
	LogIndex     int // -1 for native ETH
	TxHash       string
	DepositType  string // "eth" or "erc20"
	TokenAddress string
}

type blockScanResult struct {
	Deposits []detectedDeposit
	BlockRaw json.RawMessage // full block JSON for proof construction
}

// blockScanEntry pairs a scanBlock result with its error for the parallel
// prefetch path (MED-47). A nil err means the result is usable.
type blockScanEntry struct {
	result *blockScanResult
	err    error
}

// scanBlocksParallel prefetches scanBlock results for the inclusive block range
// [from, to] concurrently using a bounded worker pool, and returns them keyed by
// block height. MED-47 (EVM-B-M7): scanBlock is read-only on the RPC and
// independent per block, so the heavy serial RPC latency can be parallelized.
// The CALLER still consumes results in strict ascending block order and stops at
// the first error/gap, preserving the original checkpoint-advancement semantics
// (advance only through the highest contiguous successfully-scanned block).
func scanBlocksParallel(ctx context.Context, rpc *ethRPC, from, to uint64, cfg EVMBotConfig) map[uint64]blockScanEntry {
	out := make(map[uint64]blockScanEntry)
	if to < from {
		return out
	}

	// Bounded concurrency so a wide window cannot open thousands of sockets.
	const maxScanWorkers = 8
	workers := maxScanWorkers
	if span := int(to - from + 1); span < workers {
		workers = span
	}

	heights := make(chan uint64)
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for h := range heights {
				// Honor cancellation so a timed-out tick stops issuing RPC.
				if ctx.Err() != nil {
					return
				}
				// C-ER1 (round3): runOnceSafe's recover() only catches panics in
				// the tick goroutine — Go's recover() does NOT cross goroutine
				// boundaries. A panic in this worker (scanBlock parses the most
				// attacker-influenced data: hostile RPC block/log/receipt JSON)
				// would otherwise crash the WHOLE bot process, bypassing the
				// MED-46 recover entirely. Wrap each per-height scan in its own
				// recover so a worker panic degrades to a per-block ERROR (the
				// caller stops at the first error/gap and retries the window)
				// rather than killing the bridge. Never record a silent success:
				// on panic we write an error entry, so the block is treated as
				// failed and re-scanned, not skipped.
				res, err := scanBlockSafe(rpc, h, cfg.VaultAddress, cfg.Tokens)
				mu.Lock()
				out[h] = blockScanEntry{result: res, err: err}
				mu.Unlock()
			}
		}()
	}

feed:
	for h := from; h <= to; h++ {
		select {
		case <-ctx.Done():
			break feed
		case heights <- h:
		}
	}
	close(heights)
	wg.Wait()
	return out
}

// scanBlockSafe wraps scanBlock with a per-height recover (C-ER1, round3). A
// panic inside a worker goroutine cannot be caught by runOnceSafe's recover
// (recover is per-goroutine), so without this a malformed-RPC-induced panic in
// scanBlock would crash the whole bot. On panic we return a non-nil error so
// the block is treated as failed (re-scanned), never a silent success.
func scanBlockSafe(rpc *ethRPC, height uint64, vaultAddr string, tokens map[string]string) (res *blockScanResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			res = nil
			err = fmt.Errorf("scanBlock panic at height %d: %v", height, r)
		}
	}()
	return scanBlock(rpc, height, vaultAddr, tokens)
}

func scanBlock(rpc *ethRPC, height uint64, vaultAddr string, tokens map[string]string) (*blockScanResult, error) {
	blockHex := fmt.Sprintf("0x%x", height)
	blockData, err := rpc.call("eth_getBlockByNumber", fmt.Sprintf(`"%s", true`, blockHex))
	if err != nil {
		return nil, fmt.Errorf("fetch block %d: %w", height, err)
	}

	var block struct {
		Transactions []struct {
			Hash  string `json:"hash"`
			From  string `json:"from"`
			To    string `json:"to"`
			Value string `json:"value"`
		} `json:"transactions"`
	}
	if err := json.Unmarshal(blockData, &block); err != nil {
		return nil, fmt.Errorf("parse block %d: %w", height, err)
	}

	result := &blockScanResult{BlockRaw: blockData}
	vaultLower := strings.ToLower(vaultAddr)

	// Detect ETH deposits: direct transfers to vault
	for i, tx := range block.Transactions {
		if strings.ToLower(tx.To) == vaultLower && hexToUint64(tx.Value) > 0 {
			result.Deposits = append(result.Deposits, detectedDeposit{
				BlockHeight: height,
				TxIndex:     i,
				LogIndex:    -1,
				TxHash:      tx.Hash,
				DepositType: "eth",
			})
		}
	}

	// Detect ERC-20 deposits: Transfer events to vault
	for tokenAddr := range tokens {
		vaultPadded := "0x000000000000000000000000" + strings.TrimPrefix(vaultLower, "0x")
		logsParams := fmt.Sprintf(
			`{"fromBlock":"0x%x","toBlock":"0x%x","address":"%s","topics":["0x%s",null,"%s"]}`,
			height, height, tokenAddr, TransferEventSig, vaultPadded,
		)
		logsData, err := rpc.call("eth_getLogs", logsParams)
		if err != nil {
			slog.Warn("eth_getLogs failed", "token", tokenAddr, "block", height, "err", err)
			continue
		}

		var logs []struct {
			TransactionIndex string `json:"transactionIndex"`
			TransactionHash  string `json:"transactionHash"`
			LogIndex         string `json:"logIndex"`
		}
		json.Unmarshal(logsData, &logs)

		for _, log := range logs {
			// W4-A CRIT #14 + F2 hardening: the contract reader expects the
			// PER-RECEIPT log position (0..N-1 within the receipt's own logs
			// list), but eth_getLogs returns the BLOCK-LEVEL logIndex.
			// Resolve the per-receipt position by finding the SPECIFIC log
			// in the receipt that matches this block-level index. A tx
			// emitting two Transfer(token -> vault) logs previously had
			// both deposits collide on observed-index 0; matching by the
			// block-level index gives each deposit a distinct per-receipt
			// position.
			if log.LogIndex == "" {
				slog.Warn("eth_getLogs returned a log with no logIndex (non-standard RPC); skipping deposit",
					"tx", log.TransactionHash, "token", tokenAddr)
				continue
			}
			perReceiptIdx, lookupErr := lookupPerReceiptLogIndex(rpc, log.TransactionHash, tokenAddr, vaultPadded, hexToUint64(log.LogIndex))
			if lookupErr != nil {
				slog.Warn("CRIT #14: per-receipt logIndex lookup failed; skipping deposit",
					"tx", log.TransactionHash, "token", tokenAddr, "err", lookupErr)
				continue
			}
			result.Deposits = append(result.Deposits, detectedDeposit{
				BlockHeight:  height,
				TxIndex:      int(hexToUint64(log.TransactionIndex)),
				LogIndex:     perReceiptIdx,
				TxHash:       log.TransactionHash,
				DepositType:  "erc20",
				TokenAddress: tokenAddr,
			})
		}
	}

	return result, nil
}

// lookupPerReceiptLogIndex resolves the per-receipt logIndex (0..N-1 within
// the receipt's own Logs list) for a specific log identified by its block-
// level logIndex. eth_getLogs returns block-level indices; the contract
// reader expects per-receipt. Without this resolver a multi-Transfer-log
// tx would map every deposit to per-receipt position 0 (W4-A CRIT #14), or
// to the first matching position if a naive scan is used (F2-bot variant
// of the same bug).
func lookupPerReceiptLogIndex(rpc *ethRPC, txHash, tokenAddr, vaultPaddedTopic string, blockLogIndex uint64) (int, error) {
	receiptData, err := rpc.getReceipt(txHash)
	if err != nil {
		return -1, fmt.Errorf("getReceipt %s: %w", txHash, err)
	}
	var receipt struct {
		Logs []struct {
			Address  string   `json:"address"`
			Topics   []string `json:"topics"`
			LogIndex string   `json:"logIndex"` // block-level index
		} `json:"logs"`
	}
	if err := json.Unmarshal(receiptData, &receipt); err != nil {
		return -1, fmt.Errorf("decode receipt %s: %w", txHash, err)
	}
	wantedAddr := strings.ToLower(strings.TrimPrefix(tokenAddr, "0x"))
	wantedTopic := strings.ToLower(vaultPaddedTopic)
	// F2: resolve the per-receipt array position of the SPECIFIC block-level
	// log (matched by its block-level logIndex), NOT the first matching log.
	for i, log := range receipt.Logs {
		// F2 hardening: a non-standard RPC omitting logIndex would make
		// hexToUint64("")==0 silently match only block index 0 — surface a
		// distinct, retryable error instead of mis-resolving the position.
		if log.LogIndex == "" {
			return -1, fmt.Errorf("receipt %s log lacks logIndex (non-standard RPC); cannot resolve per-receipt index", txHash)
		}
		if hexToUint64(log.LogIndex) != blockLogIndex {
			continue
		}
		// Sanity-check the resolved log really is the expected Transfer(token -> vault).
		if strings.ToLower(strings.TrimPrefix(log.Address, "0x")) != wantedAddr ||
			len(log.Topics) < 3 ||
			strings.ToLower(strings.TrimPrefix(log.Topics[0], "0x")) != TransferEventSig ||
			strings.ToLower(log.Topics[2]) != wantedTopic {
			return -1, fmt.Errorf("log at block-level index %d in receipt %s is not the expected Transfer(token=%s -> vault)", blockLogIndex, txHash, tokenAddr)
		}
		return i, nil
	}
	return -1, fmt.Errorf("log with block-level index %d not found in receipt %s for token %s", blockLogIndex, txHash, tokenAddr)
}

// buildMapPayload constructs the JSON for a "map" contract call.
func buildMapPayload(deposit detectedDeposit, receiptRLP []byte, proofNodes [][]byte) json.RawMessage {
	proofHex := ""
	for _, node := range proofNodes {
		proofHex += hex.EncodeToString(node)
	}

	txData := map[string]interface{}{
		"block_height":     deposit.BlockHeight,
		"tx_index":         deposit.TxIndex,
		"raw_hex":          hex.EncodeToString(receiptRLP),
		"merkle_proof_hex": proofHex,
		"deposit_type":     deposit.DepositType,
	}
	if deposit.DepositType == "erc20" {
		txData["log_index"] = deposit.LogIndex
		txData["token_address"] = deposit.TokenAddress
	}

	payload := map[string]interface{}{
		"tx_data":      txData,
		"instructions": []string{},
	}
	data, _ := json.Marshal(payload)
	return data
}

// ---------------------------------------------------------------------------
// Withdrawal pipeline — getTssRequests → AttachSignature → broadcast
// ---------------------------------------------------------------------------

// PendingSpend mirrors the contract's PendingSpend stored at state key d-{nonce}.
//
// H-F1 (round3): the contract stores this as JSON (contract/mapping/withdrawal.go
// StorePendingSpend → json.Marshal; v18 §AE migration from pipe-delimited). The
// JSON tags here MUST match the contract struct (withdrawal.go:76-91) EXACTLY:
//
//	nonce, amount, from, to, asset, unsigned_tx_hex, block_height,
//	vault_at_queue, token_address (omitempty)
//
// The old bot parser did strings.Split(raw, "|"), which on a JSON string yields
// ONE field → len<7 → nil → every withdrawal wedged. VaultAtQueue was also
// entirely absent from the bot struct (the contract ecrecovers confirmSpend
// against it). Both are now present.
type PendingSpend struct {
	Nonce         uint64 `json:"nonce"`
	Amount        int64  `json:"amount"`
	From          string `json:"from"`
	To            string `json:"to"`
	Asset         string `json:"asset"`
	UnsignedTxHex string `json:"unsigned_tx_hex"`
	BlockHeight   uint64 `json:"block_height"`
	VaultAtQueue  string `json:"vault_at_queue"`
	TokenAddress  string `json:"token_address,omitempty"`
}

// parsePendingSpend decodes the contract's JSON-encoded PendingSpend (H-F1).
// Returns nil on a malformed/empty entry or a non-positive Amount / empty
// UnsignedTxHex (the same validation the prior pipe parser enforced), so the
// caller routes to "no usable pending spend" rather than assembling a bad tx.
func parsePendingSpend(nonce uint64, raw string) *PendingSpend {
	ps := &PendingSpend{}
	if err := json.Unmarshal([]byte(raw), ps); err != nil {
		return nil
	}
	// The contract keys the entry by nonce; trust the caller's nonce over any
	// in-band value (mirrors contract GetPendingSpend which sets ps.Nonce=nonce).
	ps.Nonce = nonce
	if ps.Amount <= 0 {
		return nil
	}
	if ps.UnsignedTxHex == "" {
		return nil
	}
	return ps
}

// parseDERSignature extracts r, s from a DER-encoded ECDSA signature.
func parseDERSignature(der []byte) (r, s []byte, err error) {
	if len(der) < 8 || der[0] != 0x30 {
		return nil, nil, fmt.Errorf("not a DER signature: len=%d", len(der))
	}
	// MED-72 (T4): support both short-form and long-form DER length encoding of
	// the outer SEQUENCE. secp256k1 sigs are always 70-72B (short form) today,
	// but a long-form length (high bit of the length byte set) was previously
	// mis-parsed as a literal length, silently corrupting r/s extraction.
	seqLen, hdrLen, lerr := decodeDERLength(der, 1)
	if lerr != nil {
		return nil, nil, fmt.Errorf("DER sequence length: %w", lerr)
	}
	pos := 1 + hdrLen
	if pos+seqLen > len(der) {
		return nil, nil, fmt.Errorf("DER sequence length %d exceeds data length %d", seqLen, len(der))
	}

	if pos >= len(der) || der[pos] != 0x02 {
		return nil, nil, fmt.Errorf("missing INTEGER tag for r at pos %d", pos)
	}
	pos++
	if pos >= len(der) {
		return nil, nil, fmt.Errorf("truncated DER: no r length byte")
	}
	rLen, rHdr, rerr := decodeDERLength(der, pos)
	if rerr != nil {
		return nil, nil, fmt.Errorf("DER r length: %w", rerr)
	}
	pos += rHdr
	if pos+rLen > len(der) {
		return nil, nil, fmt.Errorf("r length %d exceeds remaining %d bytes", rLen, len(der)-pos)
	}
	r = der[pos : pos+rLen]
	pos += rLen

	if pos >= len(der) || der[pos] != 0x02 {
		return nil, nil, fmt.Errorf("missing INTEGER tag for s at pos %d", pos)
	}
	pos++
	if pos >= len(der) {
		return nil, nil, fmt.Errorf("truncated DER: no s length byte")
	}
	sLen, sHdr, serr := decodeDERLength(der, pos)
	if serr != nil {
		return nil, nil, fmt.Errorf("DER s length: %w", serr)
	}
	pos += sHdr
	if pos+sLen > len(der) {
		return nil, nil, fmt.Errorf("s length %d exceeds remaining %d bytes", sLen, len(der)-pos)
	}
	s = der[pos : pos+sLen]

	for len(r) > 1 && r[0] == 0 {
		r = r[1:]
	}
	for len(s) > 1 && s[0] == 0 {
		s = s[1:]
	}

	r = padLeft(r, 32)
	s = padLeft(s, 32)

	return r, s, nil
}

// decodeDERLength decodes a DER length field starting at off in der. It returns
// the decoded length value and the number of bytes the length field occupied
// (1 for short form, 1+N for long form). MED-72 (T4): handles long-form
// lengths (lengths >= 128) which the previous single-byte parse mishandled.
func decodeDERLength(der []byte, off int) (length int, hdrLen int, err error) {
	if off >= len(der) {
		return 0, 0, fmt.Errorf("truncated: no length byte at %d", off)
	}
	b := der[off]
	if b < 0x80 {
		// short form: the byte IS the length.
		return int(b), 1, nil
	}
	n := int(b & 0x7f)
	if n == 0 || n > 4 {
		// n==0 is indefinite form (not valid in DER); n>4 would overflow an int
		// length for our purposes and is far beyond any secp256k1 signature.
		return 0, 0, fmt.Errorf("unsupported long-form length-of-length %d", n)
	}
	if off+1+n > len(der) {
		return 0, 0, fmt.Errorf("truncated long-form length")
	}
	val := 0
	for i := 0; i < n; i++ {
		val = (val << 8) | int(der[off+1+i])
	}
	return val, 1 + n, nil
}

func padLeft(b []byte, size int) []byte {
	if len(b) >= size {
		return b[len(b)-size:]
	}
	padded := make([]byte, size)
	copy(padded[size-len(b):], b)
	return padded
}

// attachSignatureToTx creates a signed EIP-1559 TX from the unsigned TX bytes + (v, r, s).
func attachSignatureToTx(unsignedTxHex string, v byte, r, s []byte) (string, error) {
	unsignedBytes, err := hex.DecodeString(unsignedTxHex)
	if err != nil {
		return "", fmt.Errorf("decode unsigned tx: %w", err)
	}

	if len(unsignedBytes) < 2 {
		return "", fmt.Errorf("unsigned tx too short: %d bytes", len(unsignedBytes))
	}
	if unsignedBytes[0] != 0x02 {
		return "", fmt.Errorf("not an EIP-1559 tx (type prefix 0x%x)", unsignedBytes[0])
	}

	rlpPayload := unsignedBytes[1:] // strip 0x02 prefix

	contentStart, contentLen, err := decodeRLPListHeader(rlpPayload)
	if err != nil {
		return "", fmt.Errorf("decode unsigned RLP: %w", err)
	}
	content := rlpPayload[contentStart : contentStart+contentLen]

	vRLP := encodeRLPByte(v)
	rRLP := encodeRLPBytes(r)
	sRLP := encodeRLPBytes(s)

	signedContent := make([]byte, 0, len(content)+len(vRLP)+len(rRLP)+len(sRLP))
	signedContent = append(signedContent, content...)
	signedContent = append(signedContent, vRLP...)
	signedContent = append(signedContent, rRLP...)
	signedContent = append(signedContent, sRLP...)

	signedRLP := encodeRLPList(signedContent)

	signedTx := make([]byte, 0, 1+len(signedRLP))
	signedTx = append(signedTx, 0x02)
	signedTx = append(signedTx, signedRLP...)

	return hex.EncodeToString(signedTx), nil
}

// computeSighash computes keccak256 of the unsigned EIP-1559 TX (0x02 || RLP).
func computeSighash(unsignedTxHex string) (string, error) {
	unsignedBytes, err := hex.DecodeString(unsignedTxHex)
	if err != nil {
		return "", err
	}
	hash := keccak256(unsignedBytes)
	return hex.EncodeToString(hash), nil
}

// ---------------------------------------------------------------------------
// Minimal RLP helpers — just enough for signature attachment.
// ---------------------------------------------------------------------------

func decodeRLPListHeader(data []byte) (contentStart int, contentLen int, err error) {
	if len(data) == 0 {
		return 0, 0, fmt.Errorf("empty data")
	}
	b := data[0]
	if b >= 0xc0 && b <= 0xf7 {
		length := int(b - 0xc0)
		if 1+length > len(data) {
			return 0, 0, fmt.Errorf("short-form list length %d exceeds data length %d", length, len(data))
		}
		return 1, length, nil
	}
	if b >= 0xf8 {
		lenOfLen := int(b - 0xf7)
		if len(data) < 1+lenOfLen {
			return 0, 0, fmt.Errorf("truncated list length")
		}
		var length int
		for i := 0; i < lenOfLen; i++ {
			length = (length << 8) | int(data[1+i])
		}
		if 1+lenOfLen+length > len(data) {
			return 0, 0, fmt.Errorf("long-form list length %d exceeds data length %d", length, len(data))
		}
		return 1 + lenOfLen, length, nil
	}
	return 0, 0, fmt.Errorf("not an RLP list: prefix 0x%x", b)
}

func encodeRLPByte(v byte) []byte {
	if v == 0 {
		return []byte{0x80} // empty bytes
	}
	if v < 0x80 {
		return []byte{v}
	}
	return []byte{0x81, v}
}

func encodeRLPBytes(b []byte) []byte {
	stripped := b
	for len(stripped) > 0 && stripped[0] == 0 {
		stripped = stripped[1:]
	}
	if len(stripped) == 0 {
		return []byte{0x80}
	}
	if len(stripped) == 1 && stripped[0] < 0x80 {
		return []byte{stripped[0]}
	}
	if len(stripped) <= 55 {
		out := make([]byte, 1+len(stripped))
		out[0] = 0x80 + byte(len(stripped))
		copy(out[1:], stripped)
		return out
	}
	lenBytes := encodeLength(len(stripped))
	out := make([]byte, 1+len(lenBytes)+len(stripped))
	out[0] = 0xb7 + byte(len(lenBytes))
	copy(out[1:], lenBytes)
	copy(out[1+len(lenBytes):], stripped)
	return out
}

func encodeRLPList(content []byte) []byte {
	if len(content) <= 55 {
		out := make([]byte, 1+len(content))
		out[0] = 0xc0 + byte(len(content))
		copy(out[1:], content)
		return out
	}
	lenBytes := encodeLength(len(content))
	out := make([]byte, 1+len(lenBytes)+len(content))
	out[0] = 0xf7 + byte(len(lenBytes))
	copy(out[1:], lenBytes)
	copy(out[1+len(lenBytes):], content)
	return out
}

func encodeLength(n int) []byte {
	if n < 256 {
		return []byte{byte(n)}
	}
	if n < 65536 {
		return []byte{byte(n >> 8), byte(n)}
	}
	return []byte{byte(n >> 16), byte(n >> 8), byte(n)}
}

// ---------------------------------------------------------------------------
// Keccak256
// ---------------------------------------------------------------------------

func keccak256(data []byte) []byte {
	h := sha3.NewLegacyKeccak256()
	h.Write(data)
	return h.Sum(nil)
}

// ---------------------------------------------------------------------------
// Receipt proof construction for confirmSpend.
// ---------------------------------------------------------------------------

type receiptForProof struct {
	Status            string `json:"status"`
	CumulativeGasUsed string `json:"cumulativeGasUsed"`
	LogsBloom         string `json:"logsBloom"`
	TransactionHash   string `json:"transactionHash"`
	TransactionIndex  string `json:"transactionIndex"`
	Type              string `json:"type"`
	Logs              []struct {
		Address string   `json:"address"`
		Topics  []string `json:"topics"`
		Data    string   `json:"data"`
	} `json:"logs"`
}

// confirmSpendRequest mirrors the contract's mapping.ConfirmSpendRequest
// (contract/mapping/types.go:58-72), whose canonical wire schema is exported by
// the contract as confirmSpendSchema. H-F2 (round3): the old payload emitted a
// nested {tx_data:{block_height,tx_index,raw_hex,merkle_proof_hex}} (the DEPOSIT
// envelope), which the flat intent-bound confirmSpend ABI ignores entirely —
// every field decoded zero → confirmSpend ALWAYS reverted on the intent gate.
// The contract verifies BOTH a transactions-trie proof (tx_hex + tx_proof_hex
// against header.TransactionsRoot) AND a receipts-trie proof (receipt_hex +
// receipt_proof_hex against header.ReceiptsRoot) for the SAME tx_index, plus the
// 4 intent_* fields (verified == PendingSpend BEFORE any proof work, HIGH #13).
type confirmSpendRequest struct {
	BlockHeight     uint64 `json:"block_height"`
	TxIndex         uint64 `json:"tx_index"`
	TxHex           string `json:"tx_hex"`
	TxProofHex      string `json:"tx_proof_hex"`
	ReceiptHex      string `json:"receipt_hex"`
	ReceiptProofHex string `json:"receipt_proof_hex"`
	IntentNonce     uint64 `json:"intent_nonce"`
	IntentTo        string `json:"intent_to"`
	IntentAmount    int64  `json:"intent_amount"`
	IntentAsset     string `json:"intent_asset"`
}

// buildConfirmSpendPayload assembles the flat 10-field ConfirmSpendRequest
// (H-F2). It builds a transactions-trie proof and a receipts-trie proof for the
// withdrawal tx at txIndex, and carries the intent fields from the PendingSpend
// the bot already parsed (ps). The contract uses the same tx_index as the trie
// key for BOTH proofs (handlers.go:492,593), so a single index drives both legs.
//
// All proof hex is BARE (no 0x prefix): the contract decodes via Go's
// encoding/hex which rejects a 0x prefix (proof.go / handlers.go hex.DecodeString).
func buildConfirmSpendPayload(rpc *ethRPC, txHash string, blockHeight uint64, txIndex int, ps *PendingSpend) (json.RawMessage, error) {
	if ps == nil {
		return nil, fmt.Errorf("confirmSpend: nil pending spend (intent fields unavailable)")
	}

	blockData, err := rpc.call("eth_getBlockByNumber", fmt.Sprintf(`"0x%x", false`, blockHeight))
	if err != nil {
		return nil, fmt.Errorf("fetch block %d for confirmSpend: %w", blockHeight, err)
	}

	var block struct {
		Transactions []string `json:"transactions"`
	}
	if err := json.Unmarshal(blockData, &block); err != nil {
		return nil, fmt.Errorf("parse block %d: %w", blockHeight, err)
	}
	if txIndex < 0 || txIndex >= len(block.Transactions) {
		return nil, fmt.Errorf("confirmSpend: txIndex %d out of range (block has %d txs)", txIndex, len(block.Transactions))
	}

	// --- Transactions-trie leg: tx_hex (raw typed tx RLP) + tx_proof_hex.
	// Mirrors the deposit path (buildMapPayloadFromRPC): the raw tx bytes
	// returned by eth_getRawTransactionByHash are exactly the trie leaf value,
	// so the reconstructed root matches header.TransactionsRoot. The contract
	// compares the proven leaf byte-for-byte against tx_hex.
	rawTxs := make([][]byte, len(block.Transactions))
	for i, hash := range block.Transactions {
		rData, err := rpc.call("eth_getRawTransactionByHash", fmt.Sprintf(`"%s"`, hash))
		if err != nil {
			return nil, fmt.Errorf("fetch raw tx %d/%d for confirmSpend: %w", i, len(block.Transactions), err)
		}
		var rawHex string
		if err := json.Unmarshal(rData, &rawHex); err != nil {
			return nil, fmt.Errorf("parse raw tx %d: %w", i, err)
		}
		rawTxs[i] = hexToBytes(rawHex)
		if rawTxs[i] == nil {
			return nil, fmt.Errorf("confirmSpend: raw tx %d decoded to nil (RPC returned %q)", i, rawHex)
		}
	}
	txKeys := make([][]byte, len(rawTxs))
	for i := range txKeys {
		txKeys[i] = rlpEncodeUint64(uint64(i))
	}
	_, txProofNodes, txTargetRLP := buildMPTProof(txKeys, rawTxs, txIndex)
	if txProofNodes == nil {
		return nil, fmt.Errorf("confirmSpend tx proof construction failed: txIndex %d / %d txs", txIndex, len(rawTxs))
	}

	// --- Receipts-trie leg: receipt_hex (RLP receipt) + receipt_proof_hex.
	allReceipts := make([]receiptForProof, len(block.Transactions))
	for i, hash := range block.Transactions {
		rData, err := rpc.getReceipt(hash)
		if err != nil {
			return nil, fmt.Errorf("fetch receipt %d/%d: %w", i, len(block.Transactions), err)
		}
		if err := json.Unmarshal(rData, &allReceipts[i]); err != nil {
			return nil, fmt.Errorf("parse receipt %d: %w", i, err)
		}
	}
	encodedReceipts := make([][]byte, len(allReceipts))
	for i := range allReceipts {
		encodedReceipts[i] = encodeReceiptRLP(&allReceipts[i])
	}
	rcptKeys := make([][]byte, len(encodedReceipts))
	for i := range rcptKeys {
		rcptKeys[i] = rlpEncodeUint64(uint64(i))
	}
	_, rcptProofNodes, rcptTargetRLP := buildMPTProof(rcptKeys, encodedReceipts, txIndex)
	if rcptProofNodes == nil {
		return nil, fmt.Errorf("confirmSpend receipt proof construction failed: txIndex %d / %d receipts", txIndex, len(encodedReceipts))
	}

	txProofHex := ""
	for _, node := range txProofNodes {
		txProofHex += hex.EncodeToString(node)
	}
	rcptProofHex := ""
	for _, node := range rcptProofNodes {
		rcptProofHex += hex.EncodeToString(node)
	}

	req := confirmSpendRequest{
		BlockHeight:     blockHeight,
		TxIndex:         uint64(txIndex),
		TxHex:           hex.EncodeToString(txTargetRLP),
		TxProofHex:      txProofHex,
		ReceiptHex:      hex.EncodeToString(rcptTargetRLP),
		ReceiptProofHex: rcptProofHex,
		IntentNonce:     ps.Nonce,
		IntentTo:        ps.To,
		IntentAmount:    ps.Amount,
		IntentAsset:     ps.Asset,
	}

	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("confirmSpend marshal: %w", err)
	}
	return data, nil
}

func encodeReceiptRLP(r *receiptForProof) []byte {
	status := hexToUint64(r.Status)
	cumGas := hexToUint64(r.CumulativeGasUsed)
	bloom := hexToBytes(r.LogsBloom)

	// CC-1 (H-F2 receipt leg): receipt-trie byte-string fields (bloom, log
	// address, log topics, log data) MUST be RLP-encoded VERBATIM. The previous
	// encodeRLPBytes stripped leading zero bytes, so the reconstructed receipts
	// root != header.ReceiptsRoot and the contract receipt-proof verify
	// (handlers.go:592-599) reverted on ~every withdrawal (guaranteed for ETH:
	// a no-log receipt's all-zero 256-byte bloom became 0x80 instead of the
	// canonical 0xb9 0x01 0x00 <256B>). rlpEncodeRaw is byte-identical to the
	// contract's rlp.EncodeBytes (evm-mapping-contract/contract/rlp/encode.go:5)
	// used by the contract monitor's EncodeReceipt (monitor/receipt.go:62-102).
	// The integer fields keep their integer-RLP encoders: status via
	// encodeRLPByte (0->0x80, 1->0x01) and cumulativeGasUsed via
	// rlpEncodeUint64Bytes — both match rlp.EncodeUint64.
	logItems := make([][]byte, len(r.Logs))
	for i, log := range r.Logs {
		addr := hexToBytes(log.Address)
		topicItems := make([][]byte, len(log.Topics))
		for j, t := range log.Topics {
			topicItems[j] = rlpEncodeRaw(hexToBytes(t))
		}
		topicsList := encodeRLPList(concatBytes(topicItems...))
		data := hexToBytes(log.Data)
		logItems[i] = encodeRLPList(concatBytes(
			rlpEncodeRaw(addr),
			topicsList,
			rlpEncodeRaw(data),
		))
	}
	logsList := encodeRLPList(concatBytes(logItems...))

	receiptBody := concatBytes(
		encodeRLPByte(byte(status)),
		rlpEncodeUint64Bytes(cumGas),
		rlpEncodeRaw(bloom),
		logsList,
	)
	receiptRLP := encodeRLPList(receiptBody)

	txType := hexToUint64(r.Type)
	if txType > 0 {
		typed := make([]byte, 1+len(receiptRLP))
		typed[0] = byte(txType)
		copy(typed[1:], receiptRLP)
		return typed
	}
	return receiptRLP
}

func rlpEncodeUint64(v uint64) []byte {
	if v == 0 {
		return []byte{0x80}
	}
	if v < 128 {
		return []byte{byte(v)}
	}
	var buf [8]byte
	i := 7
	for v > 0 {
		buf[i] = byte(v)
		v >>= 8
		i--
	}
	b := buf[i+1:]
	out := make([]byte, 1+len(b))
	out[0] = 0x80 + byte(len(b))
	copy(out[1:], b)
	return out
}

func rlpEncodeUint64Bytes(v uint64) []byte {
	return rlpEncodeUint64(v)
}

// ---------------------------------------------------------------------------
// Inline MPT trie — ported from monitor/trie.go.
// ---------------------------------------------------------------------------

func rlpEncodeRaw(b []byte) []byte {
	if len(b) == 1 && b[0] <= 0x7f {
		return b
	}
	if len(b) <= 55 {
		out := make([]byte, 1+len(b))
		out[0] = 0x80 + byte(len(b))
		copy(out[1:], b)
		return out
	}
	lenBytes := encodeLength(len(b))
	out := make([]byte, 1+len(lenBytes)+len(b))
	out[0] = 0xb7 + byte(len(lenBytes))
	copy(out[1:], lenBytes)
	copy(out[1+len(lenBytes):], b)
	return out
}

func rlpEncodeRawList(items ...[]byte) []byte {
	var payload []byte
	for _, item := range items {
		payload = append(payload, item...)
	}
	return encodeRLPList(payload)
}

type mptNode interface {
	mptHash() []byte
	mptEncode() []byte
}

type mptLeaf struct {
	keyNibbles []byte
	value      []byte
}

type mptBranch struct {
	children [16]mptNode
	value    []byte
}

type mptExtension struct {
	keyNibbles []byte
	child      mptNode
}

func (n *mptLeaf) mptEncode() []byte {
	compact := nibblesToCompact(n.keyNibbles, true)
	return rlpEncodeRawList(rlpEncodeRaw(compact), rlpEncodeRaw(n.value))
}

func (n *mptLeaf) mptHash() []byte {
	enc := n.mptEncode()
	if len(enc) < 32 {
		return enc
	}
	return keccak256(enc)
}

func (n *mptBranch) mptEncode() []byte {
	items := make([][]byte, 17)
	for i := 0; i < 16; i++ {
		if n.children[i] == nil {
			items[i] = rlpEncodeRaw(nil)
		} else {
			childEnc := n.children[i].mptEncode()
			if len(childEnc) < 32 {
				items[i] = childEnc
			} else {
				items[i] = rlpEncodeRaw(keccak256(childEnc))
			}
		}
	}
	items[16] = rlpEncodeRaw(n.value)
	return rlpEncodeRawList(items...)
}

func (n *mptBranch) mptHash() []byte {
	return keccak256(n.mptEncode())
}

func (n *mptExtension) mptEncode() []byte {
	compact := nibblesToCompact(n.keyNibbles, false)
	childEnc := n.child.mptEncode()
	var childRef []byte
	if len(childEnc) < 32 {
		childRef = childEnc
	} else {
		childRef = rlpEncodeRaw(keccak256(childEnc))
	}
	return rlpEncodeRawList(rlpEncodeRaw(compact), childRef)
}

func (n *mptExtension) mptHash() []byte {
	return keccak256(n.mptEncode())
}

func nibblesToCompact(nibbles []byte, isLeaf bool) []byte {
	var prefix byte
	if isLeaf {
		prefix = 2
	}
	odd := len(nibbles) % 2
	if odd == 1 {
		prefix |= 1
	}
	var compact []byte
	if odd == 1 {
		compact = append(compact, (prefix<<4)|nibbles[0])
		for i := 1; i < len(nibbles); i += 2 {
			compact = append(compact, (nibbles[i]<<4)|nibbles[i+1])
		}
	} else {
		compact = append(compact, prefix<<4)
		for i := 0; i < len(nibbles); i += 2 {
			compact = append(compact, (nibbles[i]<<4)|nibbles[i+1])
		}
	}
	return compact
}

func keyToNibbles(key []byte) []byte {
	nibbles := make([]byte, len(key)*2)
	for i, b := range key {
		nibbles[i*2] = b >> 4
		nibbles[i*2+1] = b & 0x0f
	}
	return nibbles
}

func mptBuildTrie(keys [][]byte, values [][]byte) mptNode {
	if len(keys) == 0 {
		return nil
	}
	nibbleKeys := make([][]byte, len(keys))
	for i, k := range keys {
		nibbleKeys[i] = keyToNibbles(k)
	}
	return mptBuildNode(nibbleKeys, values, 0)
}

func mptBuildNode(keys [][]byte, values [][]byte, depth int) mptNode {
	if len(keys) == 0 {
		return nil
	}
	if len(keys) == 1 {
		return &mptLeaf{keyNibbles: keys[0][depth:], value: values[0]}
	}
	commonLen := mptCommonPrefixLen(keys, depth)
	if commonLen > 0 {
		return &mptExtension{
			keyNibbles: keys[0][depth : depth+commonLen],
			child:      mptBuildNode(keys, values, depth+commonLen),
		}
	}
	branch := &mptBranch{}
	for nibble := byte(0); nibble < 16; nibble++ {
		var subKeys [][]byte
		var subVals [][]byte
		for i, k := range keys {
			if depth < len(k) && k[depth] == nibble {
				subKeys = append(subKeys, k)
				subVals = append(subVals, values[i])
			}
		}
		if len(subKeys) > 0 {
			branch.children[nibble] = mptBuildNode(subKeys, subVals, depth+1)
		}
	}
	for i, k := range keys {
		if len(k) == depth {
			branch.value = values[i]
		}
	}
	return branch
}

func mptCommonPrefixLen(keys [][]byte, depth int) int {
	if len(keys) <= 1 {
		return 0
	}
	first := keys[0]
	maxLen := len(first) - depth
	for _, k := range keys[1:] {
		kLen := len(k) - depth
		if kLen < maxLen {
			maxLen = kLen
		}
	}
	common := 0
	for i := 0; i < maxLen; i++ {
		match := true
		for _, k := range keys[1:] {
			if k[depth+i] != first[depth+i] {
				match = false
				break
			}
		}
		if !match {
			break
		}
		common++
	}
	return common
}

func mptGenerateProof(root mptNode, key []byte) [][]byte {
	nibbles := keyToNibbles(key)
	var proof [][]byte
	mptCollectProof(root, nibbles, 0, &proof)
	return proof
}

func mptCollectProof(node mptNode, nibbles []byte, depth int, proof *[][]byte) {
	if node == nil {
		return
	}
	*proof = append(*proof, node.mptEncode())
	switch n := node.(type) {
	case *mptLeaf:
		// leaf is terminal
	case *mptBranch:
		if depth < len(nibbles) {
			child := n.children[nibbles[depth]]
			if child != nil {
				mptCollectProof(child, nibbles, depth+1, proof)
			}
		}
	case *mptExtension:
		mptCollectProof(n.child, nibbles, depth+len(n.keyNibbles), proof)
	}
}

func mptTrieRoot(root mptNode) []byte {
	if root == nil {
		return keccak256(rlpEncodeRaw(nil))
	}
	return root.mptHash()
}

func buildMPTProof(keys, values [][]byte, targetIndex int) (root []byte, proof [][]byte, targetValue []byte) {
	if targetIndex < 0 || targetIndex >= len(values) || len(keys) != len(values) {
		return nil, nil, nil
	}
	trie := mptBuildTrie(keys, values)
	root = mptTrieRoot(trie)
	targetKey := rlpEncodeUint64(uint64(targetIndex))
	proof = mptGenerateProof(trie, targetKey)
	targetValue = values[targetIndex]
	return root, proof, targetValue
}

// ---------------------------------------------------------------------------
// RunLoop — the main bot loop.
// ---------------------------------------------------------------------------

func RunLoop(ctx context.Context, cfg EVMBotConfig, ethKey *ecdsa.PrivateKey, did dids.EthDID) error {
	rpc := newEthRPC(cfg.EthRPC)
	gql := newVSCGraphQL(cfg.GraphQLURLs)
	cp := loadCheckpoint(cfg.CheckpointFile)

	// MED-91 (M32-NEW-9): take an exclusive lock so a second bot instance
	// pointed at the same checkpoint cannot double-broadcast at the same nonce.
	lockFile, err := acquireCheckpointLock(cfg.CheckpointFile)
	if err != nil {
		return fmt.Errorf("checkpoint lock: %w", err)
	}
	defer lockFile.Close()

	submitter := &l2Submitter{
		ethKey: ethKey,
		did:    did,
		gql:    gql,
		cfg:    cfg,
	}

	// EVM contract creates key with ID "primary" (BTC contract uses "main")
	tssKeyID := cfg.ContractID + "-primary"

	// MED-4 (CHAIN-V2-14): cross-validate that the TSS key's derived ETH
	// address matches the configured vault. A mismatch means every withdrawal
	// would be signed by a key the vault does not recognize → confirmSpend
	// rejects it → funds stuck. This is a non-fatal startup WARN: the pubkey
	// encoding is not guaranteed, so we never brick a working bot on a parse
	// failure — we only alarm on a confidently-derived mismatch.
	validateTssVaultBinding(ctx, gql, tssKeyID, cfg.VaultAddress)

	slog.Info("loaded checkpoint", "lastBlock", cp.LastScannedBlock, "pendingTxs", len(cp.SentWithdrawals))

	// MED-51 (F-M37-7): exponential backoff applied when the contract is paused
	// so the tick loop does not burn RC retrying against a paused contract.
	pausedBackoff := cfg.PollInterval
	const maxPausedBackoff = 10 * time.Minute

	for {
		select {
		case <-ctx.Done():
			cp.save(cfg.CheckpointFile)
			return ctx.Err()
		default:
		}

		// MED-51: if the contract is paused, skip the tick body and back off
		// exponentially instead of hammering it with RC-charged calls.
		pauseCtx, pauseCancel := context.WithTimeout(ctx, 15*time.Second)
		paused, pErr := fetchContractPaused(pauseCtx, gql, cfg.ContractID)
		pauseCancel()
		if pErr == nil && paused {
			slog.Warn("contract is paused — skipping tick and backing off",
				"backoff", pausedBackoff.String())
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(pausedBackoff):
			}
			if pausedBackoff < maxPausedBackoff {
				pausedBackoff *= 2
				if pausedBackoff > maxPausedBackoff {
					pausedBackoff = maxPausedBackoff
				}
			}
			continue
		}
		pausedBackoff = cfg.PollInterval // reset once unpaused

		loopCtx, loopCancel := context.WithTimeout(ctx, 60*time.Second)

		err := runOnceSafe(loopCtx, rpc, gql, submitter, cp, cfg, tssKeyID)
		if err != nil {
			slog.Error("loop tick failed", "err", err)
		}

		loopCancel()

		cp.save(cfg.CheckpointFile)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(cfg.PollInterval):
		}
	}
}

// runOnceSafe wraps runOnce with a panic recover. MED-46 (EVM-B-M6): a latent
// panic in any tick stage (hostile RPC, malformed data, nil deref) would
// otherwise kill the bot and halt the bridge. Recover, log, and let the loop
// retry on the next tick.
func runOnceSafe(
	ctx context.Context,
	rpc *ethRPC,
	gql *vscGraphQL,
	submitter *l2Submitter,
	cp *Checkpoint,
	cfg EVMBotConfig,
	tssKeyID string,
) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("recovered panic in runOnce: %v", r)
			slog.Error("recovered from panic in tick — bot continues", "panic", r)
		}
	}()
	return runOnce(ctx, rpc, gql, submitter, cp, cfg, tssKeyID)
}

// validateTssVaultBinding fetches the TSS key's aggregated public key and
// checks that the ETH address it derives to equals the configured vault.
// MED-4 (CHAIN-V2-14). Non-fatal: logs CRITICAL on a confident mismatch,
// DEBUG when the pubkey cannot be parsed (unknown encoding) so a working bot
// is never bricked by this check.
func validateTssVaultBinding(ctx context.Context, gql *vscGraphQL, tssKeyID, vault string) {
	qctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	data, err := gql.query(qctx,
		`query GetKey($id: String!){ getTssKey(keyId: $id){ public_key } }`,
		map[string]interface{}{"id": tssKeyID},
	)
	if err != nil {
		slog.Warn("MED-4: could not fetch TSS key to validate vault binding (continuing)", "err", err)
		return
	}
	var parsed struct {
		GetTssKey *struct {
			PublicKey string `json:"public_key"`
		} `json:"getTssKey"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil || parsed.GetTssKey == nil {
		slog.Warn("MED-4: TSS key not found / undecodable — skipping vault binding check", "keyId", tssKeyID)
		return
	}
	addr, ok := tssPubKeyToAddress(parsed.GetTssKey.PublicKey)
	if !ok {
		slog.Debug("MED-4: TSS public_key encoding not recognized — skipping vault binding check (non-fatal)",
			"keyId", tssKeyID)
		return
	}
	if !strings.EqualFold(addr, vault) {
		slog.Error("MED-4 CRITICAL: TSS-derived ETH address does NOT match configured VAULT_ADDRESS — withdrawals will be unconfirmable; verify configuration before funds move",
			"keyId", tssKeyID, "derived", addr, "vault", vault)
		return
	}
	slog.Info("MED-4: TSS-derived ETH address matches configured vault", "vault", vault)
}

// tssPubKeyToAddress derives the lowercase 0x ETH address from a hex-encoded
// secp256k1 public key (uncompressed 65-byte or compressed 33-byte). Returns
// ok=false if the encoding is not a recognized secp256k1 pubkey.
func tssPubKeyToAddress(pk string) (string, bool) {
	pk = strings.TrimPrefix(strings.TrimPrefix(pk, "0x"), "0X")
	raw, err := hex.DecodeString(pk)
	if err != nil {
		return "", false
	}
	switch len(raw) {
	case 65:
		pub, err := ethCrypto.UnmarshalPubkey(raw)
		if err != nil {
			return "", false
		}
		return strings.ToLower(ethCrypto.PubkeyToAddress(*pub).Hex()), true
	case 33:
		pub, err := ethCrypto.DecompressPubkey(raw)
		if err != nil {
			return "", false
		}
		return strings.ToLower(ethCrypto.PubkeyToAddress(*pub).Hex()), true
	default:
		return "", false
	}
}

func runOnce(
	ctx context.Context,
	rpc *ethRPC,
	gql *vscGraphQL,
	submitter *l2Submitter,
	cp *Checkpoint,
	cfg EVMBotConfig,
	tssKeyID string,
) error {
	// ---------------------------------------------------------------
	// STEP 1: Get finalized block height from Ethereum
	// ---------------------------------------------------------------
	finalized, err := rpc.getFinalizedBlock()
	if err != nil {
		return fmt.Errorf("get finalized block: %w", err)
	}

	if finalized <= cp.LastScannedBlock && len(cp.SentWithdrawals) == 0 {
		return nil // nothing to do
	}

	// ---------------------------------------------------------------
	// STEP 2: Check contract's last ingested block height.
	// ---------------------------------------------------------------
	contractHeight, err := fetchContractLastHeight(ctx, gql, cfg.ContractID)
	if err != nil {
		slog.Warn("couldn't fetch contract height, proceeding with deposits only up to finalized", "err", err)
		contractHeight = finalized
	}

	// MED-48 (F-M22-MED-4): if the contract's ingested height is behind our
	// checkpoint (e.g. the ZK verifier has stalled), the scan window below is
	// empty and the bot would silently do no deposit work. Surface a WARN so
	// the stall is observable instead of looking like a healthy idle bot.
	if err == nil && contractHeight < cp.LastScannedBlock {
		slog.Warn("contract ingested height is behind bot checkpoint — verifier may be stalled; no new deposits will be mapped this tick",
			"contractHeight", contractHeight, "checkpoint", cp.LastScannedBlock)
	}

	// ---------------------------------------------------------------
	// STEP 3: Scan new blocks for deposits
	// ---------------------------------------------------------------
	scanUpTo := finalized
	if contractHeight < scanUpTo {
		scanUpTo = contractHeight
	}
	const maxBlocksPerTick = 100
	if scanUpTo > cp.LastScannedBlock+maxBlocksPerTick {
		scanUpTo = cp.LastScannedBlock + maxBlocksPerTick
	}

	// MED-47 (EVM-B-M7): scanBlock issues many serial RPC calls per block
	// (1 eth_getBlockByNumber + N eth_getLogs + per-log receipt lookups). Done
	// strictly serially across up to maxBlocksPerTick blocks, this is the tick's
	// dominant latency and can starve the 60s loopCtx. scanBlock is read-only on
	// the RPC (http.Client is concurrency-safe) and stateless per block, so we
	// PREFETCH the scan results concurrently with a bounded worker pool, then
	// process them below in STRICT block order. The checkpoint-advancement and
	// stop-at-first-failure semantics are unchanged — only the I/O is parallel.
	scanResults := scanBlocksParallel(ctx, rpc, cp.LastScannedBlock+1, scanUpTo, cfg)

	for h := cp.LastScannedBlock + 1; h <= scanUpTo; h++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		sr, ok := scanResults[h]
		if !ok || sr.err != nil {
			if ok && sr.err != nil {
				slog.Error("scan block failed", "block", h, "err", sr.err)
			}
			break // stop scanning at the first gap/failure; retry next tick
		}
		result := sr.result

		if len(result.Deposits) == 0 {
			cp.mu.Lock()
			cp.LastScannedBlock = h
			cp.mu.Unlock()
			continue
		}

		slog.Info("deposits detected", "block", h, "count", len(result.Deposits))

		// ---------------------------------------------------------------
		// STEP 4: Build proofs and submit map calls.
		// If ANY deposit fails after retries, do NOT advance the checkpoint.
		// The entire block will be retried next tick. Re-submissions of
		// already-processed deposits are harmless — the L2 layer accepts
		// them and the contract deduplicates via the observed block list.
		// ---------------------------------------------------------------
		blockFailed := false
		for _, dep := range result.Deposits {
			payload := buildMapPayloadFromRPC(ctx, rpc, dep, h)
			if payload == nil {
				slog.Error("failed to build map payload", "block", h, "tx", dep.TxHash)
				blockFailed = true
				continue
			}

			if err := submitter.callWithRetry(ctx, cfg.ContractID, "map", payload, 3); err != nil {
				slog.Error("map submission failed — block checkpoint will NOT advance",
					"block", h, "tx", dep.TxHash, "err", err)
				blockFailed = true
			}
		}

		if blockFailed {
			cp.mu.Lock()
			cp.BlockRetries[h]++
			retries := cp.BlockRetries[h]
			cp.mu.Unlock()

			if retries >= blockRetryAlertThreshold {
				slog.Error("CRITICAL: block has been failing for multiple ticks — manual intervention required",
					"block", h,
					"retries", retries,
					"minutes_stuck", retries*int(cfg.PollInterval.Seconds())/60,
				)
			} else {
				slog.Warn("one or more deposits failed in block, will retry next tick",
					"block", h, "retries", retries)
			}
			break
		}

		cp.mu.Lock()
		delete(cp.BlockRetries, h)
		cp.mu.Unlock()

		cp.mu.Lock()
		cp.LastScannedBlock = h
		cp.mu.Unlock()
	}

	// ---------------------------------------------------------------
	// STEP 5: Handle withdrawals
	// ---------------------------------------------------------------
	handleWithdrawals(ctx, rpc, gql, cp, cfg, tssKeyID)

	// ---------------------------------------------------------------
	// STEP 6: Handle confirmations
	// ---------------------------------------------------------------
	handleConfirmations(ctx, rpc, gql, submitter, cp, cfg)

	return nil
}

func handleWithdrawals(
	ctx context.Context,
	rpc *ethRPC,
	gql *vscGraphQL,
	cp *Checkpoint,
	cfg EVMBotConfig,
	tssKeyID string,
) {
	state, err := gql.fetchContractState(ctx, cfg.ContractID, []string{"n", "np"})
	if err != nil {
		slog.Debug("fetch nonce state failed", "err", err)
		return
	}

	confirmedNonce, _ := strconv.ParseUint(state["n"], 10, 64)
	pendingNonce, _ := strconv.ParseUint(state["np"], 10, 64)

	if pendingNonce <= confirmedNonce {
		return
	}

	nonceKey := strconv.FormatUint(confirmedNonce, 10)
	if _, alreadySent := cp.SentWithdrawals[nonceKey]; alreadySent {
		return
	}

	spendKey := "d-" + nonceKey
	spendState, err := gql.fetchContractState(ctx, cfg.ContractID, []string{spendKey})
	if err != nil {
		slog.Warn("fetch pending spend failed", "nonce", confirmedNonce, "err", err)
		return
	}

	spendData, ok := spendState[spendKey]
	if !ok || spendData == "" {
		slog.Warn("pending spend not found in contract state", "nonce", confirmedNonce)
		return
	}

	ps := parsePendingSpend(confirmedNonce, spendData)
	if ps == nil {
		slog.Error("failed to parse pending spend", "nonce", confirmedNonce, "raw", spendData)
		return
	}

	sighash, err := computeSighash(ps.UnsignedTxHex)
	if err != nil {
		slog.Error("compute sighash failed", "nonce", confirmedNonce, "err", err)
		return
	}

	slog.Info("pending withdrawal found, checking for TSS signature",
		"nonce", confirmedNonce,
		"asset", ps.Asset,
		"amount", ps.Amount,
		"to", ps.To,
		"sighash", sighash,
	)

	signatures, err := gql.fetchTssSignatures(ctx, tssKeyID, []string{sighash})
	if err != nil {
		slog.Warn("fetch TSS signatures failed", "err", err)
		return
	}

	sig, found := signatures[sighash]
	if !found {
		slog.Debug("TSS signature not ready yet", "sighash", sighash)
		return
	}

	r, s, err := parseDERSignature(sig.Bytes)
	if err != nil {
		slog.Error("parse DER signature failed", "err", err)
		return
	}

	var v byte = 0

	signedTxHex, err := attachSignatureToTx(ps.UnsignedTxHex, v, r, s)
	if err != nil {
		slog.Error("attach signature failed", "err", err)
		return
	}

	txHash, err := rpc.broadcastTx(signedTxHex)
	if err != nil {
		slog.Debug("broadcast with v=0 failed, trying v=1", "err", err)
		v = 1
		signedTxHex, err = attachSignatureToTx(ps.UnsignedTxHex, v, r, s)
		if err != nil {
			slog.Error("attach signature v=1 failed", "err", err)
			return
		}
		txHash, err = rpc.broadcastTx(signedTxHex)
		if err != nil {
			slog.Warn("broadcast failed with both v values, checking if already mined", "err", err)
			for _, tryV := range []byte{0, 1} {
				candidate, aErr := attachSignatureToTx(ps.UnsignedTxHex, tryV, r, s)
				if aErr != nil {
					continue
				}
				candidateBytes, _ := hex.DecodeString(candidate)
				candidateHash := fmt.Sprintf("0x%x", keccak256(candidateBytes))
				receiptData, rErr := rpc.getReceipt(candidateHash)
				if rErr != nil || receiptData == nil {
					continue
				}
				// MED-50 (F-M22-MED-8): do NOT trust a receipt blindly. A
				// hostile RPC can fabricate a receipt for a hash we never
				// landed, making the bot record a phantom SentTx and stall the
				// confirmation loop forever. Cross-check that the RPC also
				// returns a real transaction object for this exact hash, AND
				// that the canonical tx hash it reports matches what we asked
				// for.
				txData, txErr := rpc.getTransactionByHash(candidateHash)
				if txErr != nil || txData == nil {
					slog.Warn("MED-50: receipt present but no matching transaction object — ignoring (possible hostile RPC)",
						"candidateHash", candidateHash)
					continue
				}
				var txObj struct {
					Hash string `json:"hash"`
				}
				if json.Unmarshal(txData, &txObj) != nil ||
					!strings.EqualFold(txObj.Hash, candidateHash) {
					slog.Warn("MED-50: transaction object hash mismatch — ignoring (possible hostile RPC)",
						"candidateHash", candidateHash, "rpcHash", txObj.Hash)
					continue
				}
				var receipt struct {
					Status string `json:"status"`
				}
				if json.Unmarshal(receiptData, &receipt) != nil || receipt.Status == "" {
					continue
				}
				slog.Info("withdrawal TX already mined on chain, recovering",
					"txHash", candidateHash, "status", receipt.Status, "nonce", confirmedNonce)
				cp.mu.Lock()
				cp.SentWithdrawals[nonceKey] = SentTx{
					SignedTxHex: candidate,
					TxHash:      candidateHash,
					Nonce:       confirmedNonce,
					SentAt:      time.Now().Unix(),
				}
				cp.mu.Unlock()
				return
			}
			return
		}
	}

	slog.Info("withdrawal TX broadcast to Ethereum",
		"txHash", txHash,
		"nonce", confirmedNonce,
		"asset", ps.Asset,
		"amount", ps.Amount,
		"to", ps.To,
	)

	cp.mu.Lock()
	cp.SentWithdrawals[nonceKey] = SentTx{
		SignedTxHex: signedTxHex,
		TxHash:      txHash,
		Nonce:       confirmedNonce,
		SentAt:      time.Now().Unix(),
	}
	cp.mu.Unlock()
}

func handleConfirmations(
	ctx context.Context,
	rpc *ethRPC,
	gql *vscGraphQL,
	submitter *l2Submitter,
	cp *Checkpoint,
	cfg EVMBotConfig,
) {
	cp.mu.Lock()
	sent := make(map[string]SentTx)
	for k, v := range cp.SentWithdrawals {
		sent[k] = v
	}
	cp.mu.Unlock()

	if len(sent) == 0 {
		return
	}

	for nonceKey, stx := range sent {
		select {
		case <-ctx.Done():
			return
		default:
		}

		receiptData, err := rpc.getReceipt(stx.TxHash)
		if err != nil {
			slog.Debug("receipt not available yet", "txHash", stx.TxHash, "err", err)
			continue
		}

		var receipt struct {
			Status           string `json:"status"`
			BlockNumber      string `json:"blockNumber"`
			TransactionIndex string `json:"transactionIndex"`
		}
		if err := json.Unmarshal(receiptData, &receipt); err != nil {
			slog.Warn("parse receipt failed", "txHash", stx.TxHash, "err", err)
			continue
		}

		blockNum := hexToUint64(receipt.BlockNumber)
		txIndex := int(hexToUint64(receipt.TransactionIndex))
		status := hexToUint64(receipt.Status)

		finalized, err := rpc.getFinalizedBlock()
		if err != nil {
			continue
		}
		if blockNum > finalized {
			slog.Debug("TX mined but not yet finalized", "txHash", stx.TxHash, "block", blockNum, "finalized", finalized)
			continue
		}

		contractHeight, err := fetchContractLastHeight(ctx, gql, cfg.ContractID)
		if err != nil || contractHeight < blockNum {
			slog.Debug("contract hasn't ingested confirmation block yet",
				"txHash", stx.TxHash,
				"block", blockNum,
				"contractHeight", contractHeight,
			)
			continue
		}

		if status == 1 {
			slog.Info("withdrawal TX confirmed successfully on L1",
				"txHash", stx.TxHash,
				"block", blockNum,
			)
		} else {
			slog.Warn("withdrawal TX REVERTED on L1 — contract will refund user",
				"txHash", stx.TxHash,
				"block", blockNum,
				"status", status,
			)
		}

		// H-F2 (round3): confirmSpend is intent-bound — the contract verifies the
		// 4 intent_* fields against the PendingSpend at the confirmed nonce BEFORE
		// any proof work (HIGH #13). SentTx does not carry To/Amount/Asset, so
		// re-fetch the PendingSpend from contract state (key d-{nonce}) and parse
		// it with the JSON parser (H-F1). The intent fields MUST equal the stored
		// PendingSpend, so reading it from the contract is the authoritative source.
		spendKey := "d-" + strconv.FormatUint(stx.Nonce, 10)
		spendState, sErr := gql.fetchContractState(ctx, cfg.ContractID, []string{spendKey})
		if sErr != nil {
			slog.Warn("confirmSpend: fetch pending spend failed", "txHash", stx.TxHash, "nonce", stx.Nonce, "err", sErr)
			continue
		}
		ps := parsePendingSpend(stx.Nonce, spendState[spendKey])
		if ps == nil {
			// The pending spend is gone (nonce already advanced/cleared) or
			// unparseable. Don't fabricate intent fields — skip and let the
			// nonce-advance reconciliation below clear stale entries.
			slog.Warn("confirmSpend: pending spend missing/unparseable — skipping",
				"txHash", stx.TxHash, "nonce", stx.Nonce, "raw", spendState[spendKey])
			continue
		}

		payload, err := buildConfirmSpendPayload(rpc, stx.TxHash, blockNum, txIndex, ps)
		if err != nil {
			slog.Error("build confirmSpend payload failed", "txHash", stx.TxHash, "err", err)
			continue
		}

		// Submit confirmSpend via L2 with retry.
		if err := submitter.callWithRetry(ctx, cfg.ContractID, "confirmSpend", payload, 3); err != nil {
			slog.Error("confirmSpend submission failed", "txHash", stx.TxHash, "err", err)
			// Check if nonce already advanced (another instance confirmed it).
			// If so, clear the stale entry to stop perpetual retries.
			state, qErr := gql.fetchContractState(ctx, cfg.ContractID, []string{"n"})
			if qErr == nil {
				confirmedNonce, _ := strconv.ParseUint(state["n"], 10, 64)
				if confirmedNonce > stx.Nonce {
					slog.Info("nonce already advanced past this withdrawal — clearing stale entry",
						"txHash", stx.TxHash, "staleNonce", stx.Nonce, "confirmedNonce", confirmedNonce)
					cp.mu.Lock()
					delete(cp.SentWithdrawals, nonceKey)
					cp.mu.Unlock()
				}
			}
			continue
		}

		cp.mu.Lock()
		delete(cp.SentWithdrawals, nonceKey)
		cp.mu.Unlock()

		slog.Info("confirmSpend submitted", "txHash", stx.TxHash, "nonce", stx.Nonce)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func fetchContractLastHeight(ctx context.Context, gql *vscGraphQL, contractID string) (uint64, error) {
	state, err := gql.fetchContractState(ctx, contractID, []string{"h"})
	if err != nil {
		return 0, err
	}
	h, _ := strconv.ParseUint(state["h"], 10, 64)
	return h, nil
}

func buildMapPayloadFromRPC(ctx context.Context, rpc *ethRPC, dep detectedDeposit, blockHeight uint64) json.RawMessage {
	blockData, err := rpc.call("eth_getBlockByNumber", fmt.Sprintf(`"0x%x", false`, blockHeight))
	if err != nil {
		slog.Error("fetch block for proof", "block", blockHeight, "err", err)
		return nil
	}

	var block struct {
		Transactions []string `json:"transactions"`
	}
	if err := json.Unmarshal(blockData, &block); err != nil {
		slog.Error("parse block for proof", "block", blockHeight, "err", err)
		return nil
	}

	rawTxs := make([][]byte, len(block.Transactions))
	for i, hash := range block.Transactions {
		rData, err := rpc.call("eth_getRawTransactionByHash", fmt.Sprintf(`"%s"`, hash))
		if err != nil {
			slog.Error("fetch raw tx for proof", "block", blockHeight, "tx", i, "err", err)
			return nil
		}
		var rawHex string
		json.Unmarshal(rData, &rawHex)
		rawTxs[i] = hexToBytes(rawHex)
	}

	keys := make([][]byte, len(rawTxs))
	for i := range keys {
		keys[i] = rlpEncodeUint64(uint64(i))
	}

	_, proofNodes, targetRLP := buildMPTProof(keys, rawTxs, dep.TxIndex)
	if proofNodes == nil {
		slog.Error("proof construction failed", "block", blockHeight, "txIndex", dep.TxIndex, "txCount", len(rawTxs))
		return nil
	}

	return buildMapPayload(dep, targetRLP, proofNodes)
}

func hexToUint64(s string) uint64 {
	s = strings.TrimPrefix(s, "0x")
	var v uint64
	for _, c := range s {
		v <<= 4
		switch {
		case c >= '0' && c <= '9':
			v |= uint64(c - '0')
		case c >= 'a' && c <= 'f':
			v |= uint64(c - 'a' + 10)
		case c >= 'A' && c <= 'F':
			v |= uint64(c - 'A' + 10)
		}
	}
	return v
}

func hexToBytes(s string) []byte {
	s = strings.TrimPrefix(s, "0x")
	b, _ := hex.DecodeString(s)
	return b
}

func concatBytes(slices ...[]byte) []byte {
	var total int
	for _, s := range slices {
		total += len(s)
	}
	out := make([]byte, 0, total)
	for _, s := range slices {
		out = append(out, s...)
	}
	return out
}
