package mapper

import (
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
	"vsc-node/cmd/mapping-bot/database"
	"vsc-node/cmd/mapping-bot/mempool"
	"vsc-node/modules/common"
	"vsc-node/modules/hive/streamer"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/hasura/go-graphql-client"
)

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

	mu              sync.RWMutex
	lastBlockAt     time.Time
	lastBlockHeight uint32
}

func (b *Bot) setLastBlock(height uint32) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lastBlockHeight = height
	b.lastBlockAt = time.Now()
}

// LastBlock returns the most recently processed block height and when it was processed.
// Returns a zero time if no block has been processed yet this session.
func (b *Bot) LastBlock() (height uint32, at time.Time) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.lastBlockHeight, b.lastBlockAt
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
		Db:             db,
		GqlClient:      graphql.NewClient(mappingBotConfig.Get().ConnectedGraphQLAddr, nil),
		L:              slog.Default(),
		ChainParams:    chainParams,
		MempoolClient:  mempoolClient,
		NetId:          vscNetId,
		ContractId:     mappingBotConfig.Get().ContractId,
		IdentityConfig: identityConfig,
		HiveConfig:     hiveConfig,
	}, nil
}
