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

	return cfg
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
	mu               sync.Mutex
}

const blockRetryAlertThreshold = 10

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
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cp
	}
	json.Unmarshal(data, cp)
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
	data, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		slog.Error("checkpoint marshal failed", "err", err)
		return
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		slog.Error("checkpoint write failed", "path", tmpPath, "err", err)
		return
	}
	if err := os.Rename(tmpPath, path); err != nil {
		slog.Error("checkpoint rename failed", "err", err)
	}
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

func (e *ethRPC) call(method string, params string) (json.RawMessage, error) {
	body := fmt.Sprintf(`{"jsonrpc":"2.0","method":"%s","params":[%s],"id":1}`, method, params)
	resp, err := e.client.Post(e.url, "application/json", strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var result struct {
		Result json.RawMessage        `json:"result"`
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

func (e *ethRPC) broadcastTx(signedTxHex string) (string, error) {
	data, err := e.call("eth_sendRawTransaction", fmt.Sprintf(`"0x%s"`, signedTxHex))
	if err != nil {
		return "", err
	}
	var txHash string
	json.Unmarshal(data, &txHash)
	return txHash, nil
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
	for _, url := range g.urls {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := g.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			lastErr = fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
			continue
		}

		var result struct {
			Data   json.RawMessage `json:"data"`
			Errors []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			lastErr = fmt.Errorf("decode: %w", err)
			continue
		}
		if len(result.Errors) > 0 {
			return nil, fmt.Errorf("graphql error: %s", result.Errors[0].Message)
		}
		return result.Data, nil
	}
	return nil, fmt.Errorf("all graphql endpoints failed: %w", lastErr)
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
	for _, url := range g.urls {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := g.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			lastErr = fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
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
	for _, url := range g.urls {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := g.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			lastErr = fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
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
	call := &transactionpool.VscContractCall{
		ContractId: contractID,
		Action:     action,
		Payload:    string(payload),
		RcLimit:    uint(rcLimit),
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
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		_, err := s.callContractL2(ctx, contractID, action, payload)
		if err == nil {
			return nil
		}
		lastErr = err
		slog.Warn("L2 submission failed",
			"action", action,
			"attempt", attempt,
			"maxAttempts", maxAttempts,
			"err", err,
		)
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

// ---------------------------------------------------------------------------
// Deposit scanning
// ---------------------------------------------------------------------------

// TransferEventSig is keccak256("Transfer(address,address,uint256)")
const TransferEventSig = "ddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"

type detectedDeposit struct {
	BlockHeight  uint64
	TxIndex      int
	LogIndex     int    // -1 for native ETH
	TxHash       string
	DepositType  string // "eth" or "erc20"
	TokenAddress string
}

type blockScanResult struct {
	Deposits []detectedDeposit
	BlockRaw json.RawMessage // full block JSON for proof construction
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
			result.Deposits = append(result.Deposits, detectedDeposit{
				BlockHeight:  height,
				TxIndex:      int(hexToUint64(log.TransactionIndex)),
				LogIndex:     int(hexToUint64(log.LogIndex)),
				TxHash:       log.TransactionHash,
				DepositType:  "erc20",
				TokenAddress: tokenAddr,
			})
		}
	}

	return result, nil
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
type PendingSpend struct {
	Nonce         uint64
	From          string
	To            string
	Asset         string
	Amount        int64
	UnsignedTxHex string
	BlockHeight   uint64
	TokenAddress  string
}

func parsePendingSpend(nonce uint64, raw string) *PendingSpend {
	fields := strings.Split(raw, "|")
	if len(fields) < 6 {
		return nil
	}
	ps := &PendingSpend{Nonce: nonce}
	ps.From = fields[0]
	ps.To = fields[1]
	ps.Asset = fields[2]
	ps.Amount, _ = strconv.ParseInt(fields[3], 10, 64)
	if ps.Amount <= 0 {
		return nil
	}
	ps.UnsignedTxHex = fields[4]
	if ps.UnsignedTxHex == "" {
		return nil
	}
	ps.BlockHeight, _ = strconv.ParseUint(fields[5], 10, 64)
	if len(fields) >= 7 {
		ps.TokenAddress = fields[6]
	}
	return ps
}

// parseDERSignature extracts r, s from a DER-encoded ECDSA signature.
func parseDERSignature(der []byte) (r, s []byte, err error) {
	if len(der) < 8 || der[0] != 0x30 {
		return nil, nil, fmt.Errorf("not a DER signature: len=%d", len(der))
	}
	seqLen := int(der[1])
	if seqLen+2 > len(der) {
		return nil, nil, fmt.Errorf("DER sequence length %d exceeds data length %d", seqLen, len(der))
	}
	pos := 2

	if pos >= len(der) || der[pos] != 0x02 {
		return nil, nil, fmt.Errorf("missing INTEGER tag for r at pos %d", pos)
	}
	pos++
	if pos >= len(der) {
		return nil, nil, fmt.Errorf("truncated DER: no r length byte")
	}
	rLen := int(der[pos])
	pos++
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
	sLen := int(der[pos])
	pos++
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

func buildConfirmSpendPayload(rpc *ethRPC, txHash string, blockHeight uint64, txIndex int) (json.RawMessage, error) {
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

	keys := make([][]byte, len(encodedReceipts))
	for i := range keys {
		keys[i] = rlpEncodeUint64(uint64(i))
	}

	_, proofNodes, targetRLP := buildMPTProof(keys, encodedReceipts, txIndex)
	if proofNodes == nil {
		return nil, fmt.Errorf("proof construction failed: txIndex %d out of range (block has %d txs)", txIndex, len(encodedReceipts))
	}

	proofHex := ""
	for _, node := range proofNodes {
		proofHex += hex.EncodeToString(node)
	}

	payload := map[string]interface{}{
		"tx_data": map[string]interface{}{
			"block_height":     blockHeight,
			"tx_index":         txIndex,
			"raw_hex":          hex.EncodeToString(targetRLP),
			"merkle_proof_hex": proofHex,
		},
	}

	data, _ := json.Marshal(payload)
	return data, nil
}

func encodeReceiptRLP(r *receiptForProof) []byte {
	status := hexToUint64(r.Status)
	cumGas := hexToUint64(r.CumulativeGasUsed)
	bloom := hexToBytes(r.LogsBloom)

	logItems := make([][]byte, len(r.Logs))
	for i, log := range r.Logs {
		addr := hexToBytes(log.Address)
		topicItems := make([][]byte, len(log.Topics))
		for j, t := range log.Topics {
			topicItems[j] = encodeRLPBytes(hexToBytes(t))
		}
		topicsList := encodeRLPList(concatBytes(topicItems...))
		data := hexToBytes(log.Data)
		logItems[i] = encodeRLPList(concatBytes(
			encodeRLPBytes(addr),
			topicsList,
			encodeRLPBytes(data),
		))
	}
	logsList := encodeRLPList(concatBytes(logItems...))

	receiptBody := concatBytes(
		encodeRLPByte(byte(status)),
		rlpEncodeUint64Bytes(cumGas),
		encodeRLPBytes(bloom),
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

	submitter := &l2Submitter{
		ethKey: ethKey,
		did:    did,
		gql:    gql,
		cfg:    cfg,
	}

	// EVM contract creates key with ID "primary" (BTC contract uses "main")
	tssKeyID := cfg.ContractID + "-primary"

	slog.Info("loaded checkpoint", "lastBlock", cp.LastScannedBlock, "pendingTxs", len(cp.SentWithdrawals))

	for {
		select {
		case <-ctx.Done():
			cp.save(cfg.CheckpointFile)
			return ctx.Err()
		default:
		}

		loopCtx, loopCancel := context.WithTimeout(ctx, 60*time.Second)

		err := runOnce(loopCtx, rpc, gql, submitter, cp, cfg, tssKeyID)
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

	for h := cp.LastScannedBlock + 1; h <= scanUpTo; h++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		result, err := scanBlock(rpc, h, cfg.VaultAddress, cfg.Tokens)
		if err != nil {
			slog.Error("scan block failed", "block", h, "err", err)
			break // stop scanning, retry next tick
		}

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
					TxHash:     candidateHash,
					Nonce:      confirmedNonce,
					SentAt:     time.Now().Unix(),
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
		TxHash:     txHash,
		Nonce:      confirmedNonce,
		SentAt:     time.Now().Unix(),
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

		payload, err := buildConfirmSpendPayload(rpc, stx.TxHash, blockNum, txIndex)
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
