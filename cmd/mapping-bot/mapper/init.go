package mapper

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"sync"
	"sync/atomic"
	"time"
	"vsc-node/cmd/mapping-bot/chain"
	"vsc-node/cmd/mapping-bot/database"
	"vsc-node/lib/dids"
	systemconfig "vsc-node/modules/common/system-config"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/hasura/go-graphql-client"
)

type loggingTransport struct {
	transport http.RoundTripper
}

func (t *loggingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	reqDump, _ := httputil.DumpRequestOut(req, true)
	slog.Debug("HTTP request", "dump", string(reqDump))

	resp, err := t.transport.RoundTrip(req)
	if err != nil {
		return resp, err
	}

	respDump, _ := httputil.DumpResponse(resp, true)
	slog.Debug("HTTP response", "dump", string(respDump))

	return resp, err
}

const defaultGraphQLUrl = "https://api.vsc.eco/api/v1/graphql"

// BotConfiger is the interface for bot configuration, allowing test implementations.
type BotConfiger interface {
	ContractId() string
	HttpPort() uint16
	SignApiKey() string
	FilePath() string
}

type Bot struct {
	Db           *database.Database
	L            *slog.Logger
	Chain        *chain.ChainConfig // chain-specific client, parser, address generator
	ChainParams  *chaincfg.Params   // convenience alias for Chain.ChainParams
	BotConfig    BotConfiger
	SystemConfig systemconfig.SystemConfig

	// Optional interface overrides for testing. When nil, the Bot uses its own
	// concrete implementations (gqlClients, callContract, Db.State, Db.Addresses).
	Gql     GraphQLFetcher
	Caller  ContractCaller
	StateDB StateStore
	AddrDB  AddressStore

	// GqlClient and GqlURL are single-endpoint overrides used in tests.
	// When set, they take priority over gqlClients/gqlURLs respectively.
	GqlClient *graphql.Client
	GqlURL    string

	// gqlURLs and gqlClients are the ordered list of VSC node GraphQL endpoints.
	// The first entry is tried first; subsequent entries are fallbacks.
	gqlURLs    []string
	gqlClients []*graphql.Client

	lastBlockHeight atomic.Uint64
	lastBlockAt     atomic.Int64 // Unix nanoseconds; 0 means not yet set

	failedTxsMu sync.Mutex
	failedTxs   []FailedTx // VSC transactions that reached FAILED status

	// L2 submission identity — secp256k1 key + derived did:pkh:eip155 DID.
	botEthKey *ecdsa.PrivateKey
	botEthDID dids.EthDID
}

// FailedTx records a VSC transaction that reached the FAILED status.
type FailedTx struct {
	TxId   string    `json:"txId"`
	Action string    `json:"action"`
	At     time.Time `json:"at"`
}

func (b *Bot) recordFailedTx(txId, action string) {
	b.failedTxsMu.Lock()
	defer b.failedTxsMu.Unlock()
	b.failedTxs = append(b.failedTxs, FailedTx{TxId: txId, Action: action, At: time.Now()})
}

// clearFailedTxs removes previously recorded failures by their VSC tx IDs.
// Called when a retry of the same logical operation eventually succeeds.
func (b *Bot) clearFailedTxs(txIds []string) {
	b.failedTxsMu.Lock()
	defer b.failedTxsMu.Unlock()
	remove := make(map[string]struct{}, len(txIds))
	for _, id := range txIds {
		remove[id] = struct{}{}
	}
	filtered := make([]FailedTx, 0, len(b.failedTxs))
	for _, ft := range b.failedTxs {
		if _, ok := remove[ft.TxId]; !ok {
			filtered = append(filtered, ft)
		}
	}
	b.failedTxs = filtered
}

// FailedTxs returns a snapshot of VSC transactions that reached FAILED status.
func (b *Bot) FailedTxs() []FailedTx {
	b.failedTxsMu.Lock()
	defer b.failedTxsMu.Unlock()
	out := make([]FailedTx, len(b.failedTxs))
	copy(out, b.failedTxs)
	return out
}

// gql returns the GraphQLFetcher to use — the override if set, otherwise the Bot itself.
func (b *Bot) gql() GraphQLFetcher {
	if b.Gql != nil {
		return b.Gql
	}
	return b
}

// caller returns the ContractCaller to use — the override if set, otherwise
// a wrapper around the Bot's callContract method.
func (b *Bot) caller() ContractCaller {
	if b.Caller != nil {
		return b.Caller
	}
	return &botContractCaller{b}
}

// stateDB returns the StateStore to use — the override if set, otherwise Db.State.
func (b *Bot) stateDB() StateStore {
	if b.StateDB != nil {
		return b.StateDB
	}
	return b.Db.State
}

// addrDB returns the AddressStore to use — the override if set, otherwise Db.Addresses.
func (b *Bot) addrDB() AddressStore {
	if b.AddrDB != nil {
		return b.AddrDB
	}
	return b.Db.Addresses
}

// botContractCaller wraps the Bot's callContract method to satisfy ContractCaller.
type botContractCaller struct{ b *Bot }

func (c *botContractCaller) CallContract(ctx context.Context, contractInput json.RawMessage, action string) (string, error) {
	return c.b.callContract(ctx, contractInput, action)
}

func (b *Bot) setLastBlock(height uint64) {
	b.lastBlockHeight.Store(height)
	b.lastBlockAt.Store(time.Now().UnixNano())
}

// LastBlock returns the most recently processed block height and when it was processed.
// Returns a zero time if no block has been processed yet this session.
func (b *Bot) LastBlock() (height uint64, at time.Time) {
	height = b.lastBlockHeight.Load()
	if ns := b.lastBlockAt.Load(); ns != 0 {
		at = time.Unix(0, ns)
	}
	return
}

// ErrTxFailed is returned when a VSC transaction reaches the FAILED status.
var ErrTxFailed = fmt.Errorf("VSC transaction failed")

// callWithRetry broadcasts a contract call and monitors its on-chain status.
// It only re-broadcasts when the previous attempt either failed to broadcast or
// reached the FAILED status on-chain. While a transaction is still INCLUDED or
// UNCONFIRMED it keeps polling — it never re-broadcasts over a live transaction.
func (b *Bot) callWithRetry(ctx context.Context, payload json.RawMessage, action string, maxAttempts int) error {
	var lastErr error
	var recordedFailures []string // VSC txIds recorded as failed during this call
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		txId, err := b.caller().CallContract(ctx, payload, action)
		if err != nil {
			// Broadcast itself failed — retry with backoff.
			lastErr = err
			if attempt < maxAttempts {
				backoff := time.Duration(attempt) * 2 * time.Second
				b.L.Debug("broadcast failed, retrying", "action", action, "attempt", attempt, "backoff", backoff, "error", err)
				select {
				case <-time.After(backoff):
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			continue
		}

		// Broadcast succeeded — wait for on-chain resolution.
		status, err := b.awaitTxStatus(ctx, txId, action)
		if err != nil {
			// Context cancelled or similar — stop entirely.
			return fmt.Errorf("error polling tx %s status: %w", txId, err)
		}

		switch status {
		case "CONFIRMED", "PROCESSED":
			b.L.Info("VSC transaction confirmed", "action", action, "txId", txId, "status", status)
			// A retry succeeded — clear any failures recorded during earlier attempts.
			if len(recordedFailures) > 0 {
				b.clearFailedTxs(recordedFailures)
			}
			return nil
		case "FAILED":
			b.L.Warn("VSC transaction failed on-chain", "action", action, "txId", txId, "attempt", attempt)
			b.recordFailedTx(txId, action)
			recordedFailures = append(recordedFailures, txId)
			lastErr = fmt.Errorf("%w: action=%s txId=%s", ErrTxFailed, action, txId)
			// Fall through to retry with a new broadcast.
			if attempt < maxAttempts {
				backoff := time.Duration(attempt) * 2 * time.Second
				b.L.Debug("will re-broadcast after FAILED", "action", action, "attempt", attempt, "backoff", backoff)
				select {
				case <-time.After(backoff):
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
	}
	return lastErr
}

const (
	// initialStatusDelay is how long to wait after broadcast before the first
	// status poll, giving the VSC node time to index the transaction.
	initialStatusDelay = 10 * time.Second
	// statusPollInterval is how often to re-check after the initial delay.
	statusPollInterval = 3 * time.Second
)

// awaitTxStatus waits for a broadcast transaction to reach a terminal status.
// It sleeps initialStatusDelay before the first poll, then polls every
// statusPollInterval. It never returns while the tx is INCLUDED/UNCONFIRMED —
// only CONFIRMED, PROCESSED, FAILED, or a context error will end the loop.
func (b *Bot) awaitTxStatus(ctx context.Context, txId string, action string) (string, error) {
	// Initial delay — give the node time to ingest the Hive block.
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(initialStatusDelay):
	}

	for {
		status, err := b.gql().FetchTransactionStatus(ctx, txId)
		if err != nil {
			// Transaction may not be indexed yet — keep polling.
			b.L.Debug("tx status not available yet", "action", action, "txId", txId, "error", err)
		} else {
			b.L.Debug("polled tx status", "action", action, "txId", txId, "status", status)
			switch status {
			case "CONFIRMED", "PROCESSED", "FAILED":
				return status, nil
			}
			// INCLUDED / UNCONFIRMED — keep polling.
		}

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(statusPollInterval):
		}
	}
}

// postTxWithRetry retries a transaction broadcast up to maxAttempts times with exponential backoff.
func (b *Bot) postTxWithRetry(rawTx string, maxAttempts int) error {
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		lastErr = b.Chain.Client.PostTx(rawTx)
		if lastErr == nil {
			return nil
		}
		if attempt < maxAttempts {
			backoff := time.Duration(attempt) * 2 * time.Second
			b.L.Debug("PostTx failed, retrying", "attempt", attempt, "backoff", backoff, "error", lastErr)
			time.Sleep(backoff)
		}
	}
	return lastErr
}

// NewBot creates a bot instance for the given chain configuration.
func NewBot(
	db *database.Database,
	chainCfg *chain.ChainConfig,
	mappingBotConfig MappingBotConfig,
	systemConfig systemconfig.SystemConfig,
) (*Bot, error) {
	gqlAddrs := mappingBotConfig.Get().ConnectedGraphQLAddrs
	if len(gqlAddrs) == 0 {
		return nil, fmt.Errorf("ConnectedGraphQLAddrs must have at least one entry")
	}

	gqlClients := make([]*graphql.Client, len(gqlAddrs))
	for i, addr := range gqlAddrs {
		gqlClients[i] = graphql.NewClient(addr, http.DefaultClient)
	}

	// Load or generate the bot's L2 signing key. Failure here is fatal since
	// all contract calls go through the L2 path.
	priv, generated, err := mappingBotConfig.BotEthKey()
	if err != nil {
		return nil, fmt.Errorf("L2 signing key unavailable: %w", err)
	}
	ethDID := mappingBotConfig.BotEthDID(priv)
	if generated {
		slog.Warn("generated new L2 signing key — fund this DID with HBD before the bot can submit transactions",
			"did", ethDID.String())
	}

	return &Bot{
		Db:           db,
		L:            slog.Default(),
		Chain:        chainCfg,
		ChainParams:  chainCfg.ChainParams,
		BotConfig:    mappingBotConfig,
		SystemConfig: systemConfig,
		gqlURLs:      gqlAddrs,
		gqlClients:   gqlClients,
		botEthKey:    priv,
		botEthDID:    ethDID,
	}, nil
}

