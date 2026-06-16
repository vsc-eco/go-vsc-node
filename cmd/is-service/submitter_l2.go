package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	ethCrypto "github.com/ethereum/go-ethereum/crypto"

	"vsc-node/lib/dids"
	"vsc-node/modules/db/vsc/contracts"
	transactionpool "vsc-node/modules/transaction-pool"
)

// SubmitterL2 posts mapInstantSendV2 transactions to a real VSC node via
// the submitTransactionV1 GraphQL mutation. Modeled after
// cmd/mapping-bot/mapper/call_contract_l2.go's path — the bot proves
// the same primitives work, this just wires them through the IS-service's
// fixed (contract, action) tuple.
//
// Construction needs:
//
//   - an L2-signing eth key (hex-encoded private key); the derived DID
//     must be HBD-funded to pay RC for each mapInstantSendV2 tx
//   - the VSC node's GraphQL endpoint (e.g. https://api.vsc.eco/api/v1/graphql)
//   - the dash-mapping-contract id on that net
//   - the NetId ("vsc-mainnet" / "vsc-testnet" — must match the IS
//     service's ChainID)
//
// SubmitMapInstantSend serializes-concurrent — only one L2 submission at
// a time per submitter, since nonces would otherwise race.
type SubmitterL2 struct {
	gqlEndpoint    string
	contractId     string
	netId          string
	rcLimit        int64
	httpClient     *http.Client
	privKey        *ecdsa.PrivateKey
	did            dids.EthDID
	submitMu       chanMu
	defaultTimeout time.Duration
}

// SubmitterL2Config configures the L2 submitter.
type SubmitterL2Config struct {
	// GraphQLEndpoint is the VSC node's GraphQL endpoint
	// (e.g. https://api.vsc.eco/api/v1/graphql).
	GraphQLEndpoint string
	// ContractId is the dash-mapping-contract id on this net.
	ContractId string
	// NetId is the VSC net identifier ("vsc-mainnet" / "vsc-testnet").
	NetId string
	// RcLimit is the per-tx RC budget. Spec recommends ~500 for
	// op=call, ~200 for op=auth (see estimateRcCost in the contract).
	// Set generously — overpay is fine, underpay aborts.
	RcLimit int64
	// PrivateKeyHex is the L2 signing key (hex-encoded secp256k1).
	// REQUIRED — the derived did:pkh:eip155 needs HBD to pay RC.
	PrivateKeyHex string
	// HTTPClient is the http client used to POST to the GraphQL
	// endpoint. nil → 30s timeout default.
	HTTPClient *http.Client
}

// NewSubmitterL2 constructs an L2 submitter ready to post mapInstantSendV2
// transactions to the configured VSC node.
func NewSubmitterL2(cfg SubmitterL2Config) (*SubmitterL2, error) {
	if cfg.GraphQLEndpoint == "" {
		return nil, fmt.Errorf("GraphQLEndpoint required")
	}
	if cfg.ContractId == "" {
		return nil, fmt.Errorf("ContractId required")
	}
	if cfg.NetId == "" {
		return nil, fmt.Errorf("NetId required")
	}
	if cfg.PrivateKeyHex == "" {
		return nil, fmt.Errorf("PrivateKeyHex required")
	}
	if cfg.RcLimit <= 0 {
		// 10000 RC = ~10 HBD. The op=call full pipeline (mapInstantSendV2
		// → dispatchForward → forwarder.execute → target ContractCallAs)
		// burns through the previous 1000-default cap and aborts with
		// err=gas_limit_hit errMsg="cost limit exceeded". Keep this in
		// sync with cmd/is-service/args.go:l2RcLimit default.
		cfg.RcLimit = 10000
	}
	priv, err := ethCrypto.HexToECDSA(cfg.PrivateKeyHex)
	if err != nil {
		return nil, fmt.Errorf("invalid PrivateKeyHex: %w", err)
	}
	addr := ethCrypto.PubkeyToAddress(priv.PublicKey).Hex()
	did := dids.NewEthDID(addr)
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &SubmitterL2{
		gqlEndpoint:    cfg.GraphQLEndpoint,
		contractId:     cfg.ContractId,
		netId:          cfg.NetId,
		rcLimit:        cfg.RcLimit,
		httpClient:     httpClient,
		privKey:        priv,
		did:            did,
		submitMu:       newChanMu(),
		defaultTimeout: 30 * time.Second,
	}, nil
}

// DID returns the did:pkh:eip155 ref the submitter pays RC from. Operators
// use this for the funding check ("send HBD to <DID>").
func (s *SubmitterL2) DID() string { return s.did.String() }

// SubmitMapInstantSend implements Submitter. Encodes the payload as
// CBOR via TransactionCrafter and posts to submitTransactionV1.
// Returns the L2 tx CID so the orchestrator can persist it on the
// Session (audit `submitter-l2-txid-discarded-no-observability`).
//
// Lock scope minimised (audit `submitter-mu-blocks-with-http-nonce-fetch`):
// only the nonce-fetch + broadcast pair runs under the mutex. Payload
// marshal + crafter.SignFinal run outside.
func (s *SubmitterL2) SubmitMapInstantSend(ctx context.Context, payload MapInstantSendPayload) (string, error) {
	payloadJSON, err := payload.Marshal()
	if err != nil {
		return "", fmt.Errorf("marshal payload: %w", err)
	}

	// nonce-fetch + broadcast are the only steps that must serialize.
	// Hold the mu only across them; signing the (still un-nonced) op
	// happens AFTER we've reserved the nonce.
	if err := s.submitMu.lock(ctx); err != nil {
		return "", err
	}
	defer s.submitMu.unlock()

	nonce, err := s.fetchAccountNonce(ctx, s.did.String())
	if err != nil {
		return "", fmt.Errorf("fetch L2 nonce: %w", err)
	}

	call := &transactionpool.VscContractCall{
		ContractId: s.contractId,
		Action:     "mapInstantSendV2",
		Payload:    string(payloadJSON),
		RcLimit:    uint(s.rcLimit),
		Intents:    []contracts.Intent{},
		Caller:     s.did.String(),
		NetId:      s.netId,
	}
	op, err := call.SerializeVSC()
	if err != nil {
		return "", fmt.Errorf("serialize L2 op: %w", err)
	}

	vscTx := transactionpool.VSCTransaction{
		Ops:     []transactionpool.VSCTransactionOp{op},
		Nonce:   nonce,
		NetId:   s.netId,
		RcLimit: uint64(s.rcLimit),
	}

	crafter := transactionpool.TransactionCrafter{
		Identity: dids.NewEthProvider(s.privKey),
		Did:      s.did,
	}
	sTx, err := crafter.SignFinal(vscTx)
	if err != nil {
		return "", fmt.Errorf("sign L2 tx: %w", err)
	}
	if len(sTx.Tx) > transactionpool.MAX_TX_SIZE {
		return "", fmt.Errorf("L2 tx too large: %d bytes (limit %d)", len(sTx.Tx), transactionpool.MAX_TX_SIZE)
	}

	txID, err := s.submitTransactionV1(
		ctx,
		base64.URLEncoding.EncodeToString(sTx.Tx),
		base64.URLEncoding.EncodeToString(sTx.Sig),
	)
	if err != nil {
		slog.Error("mapInstantSendV2 broadcast failed",
			"signer", s.did.String(), "nonce", nonce, "err", err)
		return "", fmt.Errorf("broadcast L2 tx: %w", err)
	}
	slog.Info("mapInstantSendV2 broadcast accepted",
		"l2TxId", txID, "signer", s.did.String(), "nonce", nonce,
		"cborSize", len(sTx.Tx))
	return txID, nil
}

// FetchTransactionStatus polls the L2 GraphQL findTransaction(id) for
// the given L2 tx CID. Returns the upstream status string or an error.
// Round-2 audit D2-DESIGN-06 — gates SessionToken issuance behind
// actual on-chain confirmation rather than mempool-accept.
func (s *SubmitterL2) FetchTransactionStatus(ctx context.Context, l2TxID string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"query":     `query($id: String!){ findTransaction(filterOptions: {byTxId: $id}){ txs { status } } }`,
		"variables": map[string]any{"id": l2TxID},
	})
	var result struct {
		Data struct {
			FindTransaction struct {
				Txs []struct {
					Status string `json:"status"`
				} `json:"txs"`
			} `json:"findTransaction"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := s.gqlPost(ctx, body, &result); err != nil {
		return "", err
	}
	if len(result.Errors) > 0 {
		return "", fmt.Errorf("graphql: %s", result.Errors[0].Message)
	}
	if len(result.Data.FindTransaction.Txs) == 0 {
		return L2StatusUnknown, nil
	}
	return result.Data.FindTransaction.Txs[0].Status, nil
}

// FetchValidatorSet reads the dash-mapping-contract's per-epoch
// validator set via the L2 GraphQL getStateByKeys query and returns
// the {ValidatorDID → PubkeyHex} map.
//
// State key layout (matches contract/mapping/forwarder_integration.go):
//
//	key:   "vs-<epoch>"
//	value: "<registeredAtBlock>#<did1>=<pk1>|<did2>=<pk2>|..."
//
// The trailing 'account' / PoP fields used at registration are NOT
// persisted; they exist only on the inbound admin payload (see
// ParseValidatorSetPayload). Round-4 audit R4-001.
//
// Returns nil (no error) when the key is unset — the orchestrator
// treats nil as "no expected set known; fall back to raw-sig verify".
func (s *SubmitterL2) FetchValidatorSet(ctx context.Context, contractID string, epoch uint64) (map[string]string, error) {
	stateKey := "vs-" + strconv.FormatUint(epoch, 10)
	body, _ := json.Marshal(map[string]any{
		"query":     `query($c: String!, $k: [String!]!){ getStateByKeys(contractId: $c, keys: $k) }`,
		"variables": map[string]any{"c": contractID, "k": []string{stateKey}},
	})
	var result struct {
		Data struct {
			GetStateByKeys map[string]any `json:"getStateByKeys"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := s.gqlPost(ctx, body, &result); err != nil {
		return nil, err
	}
	if len(result.Errors) > 0 {
		return nil, fmt.Errorf("graphql: %s", result.Errors[0].Message)
	}
	rawV, ok := result.Data.GetStateByKeys[stateKey]
	if !ok || rawV == nil {
		return nil, nil
	}
	raw, ok := rawV.(string)
	if !ok {
		return nil, fmt.Errorf("validator-set state value not a string: %T", rawV)
	}
	// Strip registeredAt prefix.
	hash := strings.IndexByte(raw, '#')
	if hash < 0 {
		return nil, fmt.Errorf("malformed validator-set state value: missing '#' prefix")
	}
	entries := strings.Split(raw[hash+1:], "|")
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		if e == "" {
			continue
		}
		parts := strings.SplitN(e, "=", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			continue
		}
		out[parts[0]] = parts[1]
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// FetchSubmitterHealth reports the submitter's HBD balance (in cents)
// and remaining RC. Used by /healthz to flag RC-exhaustion that would
// otherwise present as silent ATTESTING pile-ups. Round-4 audit R4-007.
func (s *SubmitterL2) FetchSubmitterHealth(ctx context.Context) (balanceHbdCents int64, rcRemaining int64, err error) {
	body, _ := json.Marshal(map[string]any{
		"query":     `query($a: String!){ getAccountBalance(account: $a){ hbd } getAccountRC(account: $a){ amount } }`,
		"variables": map[string]any{"a": s.did.String()},
	})
	var result struct {
		Data struct {
			GetAccountBalance struct {
				Hbd int64 `json:"hbd"`
			} `json:"getAccountBalance"`
			GetAccountRC struct {
				Amount int64 `json:"amount"`
			} `json:"getAccountRC"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if e := s.gqlPost(ctx, body, &result); e != nil {
		return 0, 0, e
	}
	if len(result.Errors) > 0 {
		return 0, 0, fmt.Errorf("graphql: %s", result.Errors[0].Message)
	}
	return result.Data.GetAccountBalance.Hbd, result.Data.GetAccountRC.Amount, nil
}

// fetchAccountNonce mirrors mapper/graphql_client.go's FetchAccountNonce.
func (s *SubmitterL2) fetchAccountNonce(ctx context.Context, account string) (uint64, error) {
	body, _ := json.Marshal(map[string]any{
		"query":     `query($a: String!){ getAccountNonce(account: $a){ nonce } }`,
		"variables": map[string]any{"a": account},
	})
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
	if err := s.gqlPost(ctx, body, &result); err != nil {
		return 0, err
	}
	if len(result.Errors) > 0 {
		return 0, fmt.Errorf("graphql: %s", result.Errors[0].Message)
	}
	return result.Data.GetAccountNonce.Nonce, nil
}

// submitTransactionV1 mirrors mapper/graphql_client.go's SubmitTransactionV1.
func (s *SubmitterL2) submitTransactionV1(ctx context.Context, txB64, sigB64 string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"query":     `query($tx: String!, $sig: String!){ submitTransactionV1(tx: $tx, sig: $sig){ id } }`,
		"variables": map[string]any{"tx": txB64, "sig": sigB64},
	})
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
	if err := s.gqlPost(ctx, body, &result); err != nil {
		return "", err
	}
	if len(result.Errors) > 0 {
		return "", fmt.Errorf("graphql: %s", result.Errors[0].Message)
	}
	if result.Data.SubmitTransactionV1.ID == nil {
		return "", fmt.Errorf("submitTransactionV1 returned nil id")
	}
	return *result.Data.SubmitTransactionV1.ID, nil
}

func (s *SubmitterL2) gqlPost(ctx context.Context, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, "POST", s.gqlEndpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("gql http %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// chanMu is a context-aware mutex via a buffered channel — lets us bail
// the wait if the context is cancelled rather than blocking forever.
type chanMu chan struct{}

func newChanMu() chanMu {
	m := make(chanMu, 1)
	m <- struct{}{}
	return m
}

func (m chanMu) lock(ctx context.Context) error {
	select {
	case <-m:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m chanMu) unlock() {
	m <- struct{}{}
}
