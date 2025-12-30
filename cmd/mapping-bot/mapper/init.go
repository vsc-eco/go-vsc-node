package mapper

import (
	"log/slog"
	"vsc-node/cmd/mapping-bot/database"

	"github.com/hasura/go-graphql-client"
)

const graphQLUrl = "https://api.vsc.eco/api/v1/graphql"

type MapperState struct {
	Db *database.Database
	// txs that have been posted, but haven't been seen in a block yet
	GqlClient *graphql.Client
	L         *slog.Logger
}

func NewMapperState(db *database.Database) (*MapperState, error) {
	return &MapperState{
		Db:        db,
		GqlClient: graphql.NewClient(graphQLUrl, nil),
		L:         slog.Default(),
	}, nil
}
