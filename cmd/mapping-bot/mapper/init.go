package mapper

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"sync/atomic"
	"time"
	"vsc-node/cmd/mapping-bot/chain"
	"vsc-node/cmd/mapping-bot/database"
	"vsc-node/modules/common"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/hive/streamer"

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
	Db             *database.Database
	GqlClient      *graphql.Client
	L              *slog.Logger
	Chain          *chain.ChainConfig // chain-specific client, parser, address generator
	ChainParams    *chaincfg.Params   // convenience alias for Chain.ChainParams
	BotConfig      BotConfiger
	IdentityConfig common.IdentityConfig
	HiveConfig     streamer.HiveConfig
	SystemConfig   systemconfig.SystemConfig

	// Optional interface overrides for testing. When nil, the Bot uses its own
	// concrete implementations (GqlClient, callContract, Db.State, Db.Addresses).
	Gql     GraphQLFetcher
	Caller  ContractCaller
	StateDB StateStore
	AddrDB  AddressStore

	lastBlockHeight atomic.Uint64
	lastBlockAt     atomic.Int64 // Unix nanoseconds; 0 means not yet set
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

func (c *botContractCaller) CallContract(ctx context.Context, contractInput json.RawMessage, action string) error {
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

// callWithRetry retries a contract call up to maxAttempts times with exponential backoff.
func (b *Bot) callWithRetry(ctx context.Context, payload json.RawMessage, action string, maxAttempts int) error {
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		lastErr = b.caller().CallContract(ctx, payload, action)
		if lastErr == nil {
			return nil
		}
		if attempt < maxAttempts {
			backoff := time.Duration(attempt) * 2 * time.Second
			b.L.Debug("contract call failed, retrying", "action", action, "attempt", attempt, "backoff", backoff, "error", lastErr)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return lastErr
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
	identityConfig common.IdentityConfig,
	hiveConfig streamer.HiveConfig,
	systemConfig systemconfig.SystemConfig,
) (*Bot, error) {
	gqlClient := graphql.NewClient(mappingBotConfig.Get().ConnectedGraphQLAddr, http.DefaultClient)

	return &Bot{
		Db:             db,
		GqlClient:      gqlClient,
		L:              slog.Default(),
		Chain:          chainCfg,
		ChainParams:    chainCfg.ChainParams,
		BotConfig:      mappingBotConfig,
		IdentityConfig: identityConfig,
		HiveConfig:     hiveConfig,
		SystemConfig:   systemConfig,
	}, nil
}
