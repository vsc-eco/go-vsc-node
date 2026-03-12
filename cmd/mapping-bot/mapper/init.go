package mapper

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"sync/atomic"
	"time"
	"vsc-node/cmd/mapping-bot/database"
	"vsc-node/cmd/mapping-bot/mempool"
	"vsc-node/modules/common"
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

type Bot struct {
	Db             *database.Database
	GqlClient      *graphql.Client
	L              *slog.Logger
	ChainParams    *chaincfg.Params
	MempoolClient  *mempool.MempoolClient
	ContractId     string
	NetId          string // vsc network id
	IdentityConfig common.IdentityConfig
	HiveConfig     streamer.HiveConfig

	lastBlockHeight atomic.Uint64
	lastBlockAt     atomic.Int64 // Unix nanoseconds; 0 means not yet set
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

func parseNetwork(name string) (*chaincfg.Params, string, error) {
	switch name {
	case "mainnet":
		return &chaincfg.MainNetParams, mempool.MempoolMainnetAPIBase, nil
	case "testnet4":
		return &chaincfg.TestNet4Params, mempool.MempoolTestnet4APIBase, nil
	case "testnet3":
		return &chaincfg.TestNet3Params, mempool.MempoolTestnet3APIBase, nil
	case "regnet":
		return &chaincfg.RegressionNetParams, mempool.MempoolMainnetAPIBase, nil
	default:
		return nil, "", fmt.Errorf("unknown network %q: must be mainnet, testnet4, testnet3, or regnet", name)
	}
}

func NewBot(
	db *database.Database,
	btcNetId string,
	vscNetId string,
	mappingBotConfig MappingBotConfig,
	identityConfig common.IdentityConfig,
	hiveConfig streamer.HiveConfig,
) (*Bot, error) {
	chainParams, mempoolBase, err := parseNetwork(btcNetId)
	if err != nil {
		return nil, err
	}

	mempoolClient := mempool.NewMempoolClient(http.DefaultClient, mempoolBase)

	return &Bot{
		Db: db,
		GqlClient: graphql.NewClient(mappingBotConfig.Get().ConnectedGraphQLAddr, &http.Client{
			Transport: &loggingTransport{http.DefaultTransport},
		}),
		L:              slog.Default(),
		ChainParams:    chainParams,
		MempoolClient:  mempoolClient,
		NetId:          vscNetId,
		ContractId:     mappingBotConfig.Get().ContractId,
		IdentityConfig: identityConfig,
		HiveConfig:     hiveConfig,
	}, nil
}
