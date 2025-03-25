package gql

import (
	"context"
	"log"
	"net/http"
	"time"

	a "vsc-node/modules/aggregate"

	"github.com/99designs/gqlgen/graphql"
	"github.com/99designs/gqlgen/graphql/handler"
	pg "github.com/99designs/gqlgen/graphql/playground"
	"github.com/chebyrash/promise"
)

// ===== constants =====

const shutdownTimeout = 5 * time.Second

// ===== types =====

type gqlManager struct {
	server *http.Server
	Addr   string
	schema graphql.ExecutableSchema
}

// ===== interface assertion =====

var _ a.Plugin = &gqlManager{}

// ===== implementing the a.Plugin interface =====

func New(schema graphql.ExecutableSchema, addr string) *gqlManager {
	return &gqlManager{
		Addr:   addr,
		schema: schema,
	}
}

func (g *gqlManager) Init() error {
	mux := http.NewServeMux()

	// creates GraphQL server with Apollo
	gqlServer := handler.New(g.schema)

	// OPTIONAL, UNCOMMENT TO ENABLE TRACING
	// gqlServer.Use(apollotracing.Tracer{})

	// adds handlers for GraphQL and Apollo sandbox environment
	mux.Handle("/api/v1/graphql", gqlServer)
	mux.Handle("/sandbox", pg.ApolloSandboxHandler("Apollo Sandbox", "/api/v1/graphql"))

	// assigns the HTTP server
	g.server = &http.Server{
		Addr:    g.Addr,
		Handler: mux,
	}

	return nil
}

func (g *gqlManager) Start() *promise.Promise[any] {
	return promise.New(func(resolve func(any), reject func(error)) {
		log.Printf("GraphQL sandbox available on %s/sandbox", g.Addr)

		if err := g.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			reject(err)
		}

		resolve(nil)
	})
}

func (g *gqlManager) Stop() error {
	log.Println("Shutting down GraphQL server...")

	// gracefully shuts down the server with a timeout
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := g.server.Shutdown(ctx); err != nil {
		return err
	}

	log.Println("GraphQL server shut down successfully")
	return nil
}
