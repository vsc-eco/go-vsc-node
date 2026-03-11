package mapper

import (
	"log/slog"
	"os"
	"sync"
	"time"
	"vsc-node/cmd/mapping-bot/database"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/hasura/go-graphql-client"
)

const defaultGraphQLUrl = "https://api.vsc.eco/api/v1/graphql"

type MapperState struct {
	Db             *database.Database
	GqlClient      *graphql.Client
	L              *slog.Logger
	ChainParams    *chaincfg.Params
	MempoolBaseURL string

	mu              sync.RWMutex
	lastBlockAt     time.Time
	lastBlockHeight uint32
}

func (ms *MapperState) setLastBlock(height uint32) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	ms.lastBlockHeight = height
	ms.lastBlockAt = time.Now()
}

// LastBlock returns the most recently processed block height and when it was processed.
// Returns a zero time if no block has been processed yet this session.
func (ms *MapperState) LastBlock() (height uint32, at time.Time) {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	return ms.lastBlockHeight, ms.lastBlockAt
}

func NewMapperState(db *database.Database, chainParams *chaincfg.Params, mempoolBaseURL string) (*MapperState, error) {
	gqlUrl := os.Getenv("VSC_GRAPHQL_URL")
	if gqlUrl == "" {
		gqlUrl = defaultGraphQLUrl
	}
	return &MapperState{
		Db:             db,
		GqlClient:      graphql.NewClient(gqlUrl, nil),
		L:              slog.Default(),
		ChainParams:    chainParams,
		MempoolBaseURL: mempoolBaseURL,
	}, nil
}
