package mapper

import (
	"log/slog"
	"os"
	"vsc-node/cmd/mapping-bot/database"

	"github.com/hasura/go-graphql-client"
)

const defaultGraphQLUrl = "https://api.vsc.eco/api/v1/graphql"

type MapperState struct {
	Db *database.Database
	// txs that have been posted, but haven't been seen in a block yet
	GqlClient *graphql.Client
	L         *slog.Logger
}

func NewMapperState(db *database.Database) (*MapperState, error) {
	gqlUrl := os.Getenv("VSC_GRAPHQL_URL")
	if gqlUrl == "" {
		gqlUrl = defaultGraphQLUrl
	}
	return &MapperState{
		Db:        db,
		GqlClient: graphql.NewClient(gqlUrl, nil),
		L:         slog.Default(),
	}, nil
}
