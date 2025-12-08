package mapper

import (
	"sync"
	"vsc-node/cmd/mapping-bot/database"

	"github.com/hasura/go-graphql-client"
)

const graphQLUrl = "https://api.vsc.eco/api/v1/graphql"

type MapperState struct {
	Mutex                sync.Mutex
	Db                   *database.Database
	AwaitingSignatureTxs *AwaitingSignature
	// txs that have been posted, but haven't been seen in a block yet
	GqlClient *graphql.Client
}

func NewMapperState(db *database.Database) (*MapperState, error) {
	unsignedTxs := &AwaitingSignature{
		Txs:    make(map[string]*SignedData),
		Hashes: make(map[string]*HashMetadata),
	}

	return &MapperState{
		Mutex:                sync.Mutex{},
		Db:                   db,
		AwaitingSignatureTxs: unsignedTxs,
		GqlClient:            graphql.NewClient(graphQLUrl, nil),
	}, nil
}
