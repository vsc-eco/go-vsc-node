package gql

import (
	"context"
	"log"
	"net/http"
	"time"

	a "vsc-node/modules/aggregate"
	"vsc-node/modules/gql/gqlgen"

	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/apollotracing"
	"github.com/99designs/gqlgen/graphql/playground"
	"github.com/chebyrash/promise"
)

// ===== constants =====

const defaultPort = "8080"
const shutdownTimeout = 5 * time.Second

// ===== types =====

type gqlManager struct {
	server *http.Server
	port   string
	stop   chan struct{}
}

// ===== interface assertion =====

var _ a.Plugin = &gqlManager{}

// ===== implementing the a.Plugin interface =====

func New() *gqlManager {
	return &gqlManager{
		port: defaultPort,
		stop: make(chan struct{}),
	}
}

func (g *gqlManager) Init() error {
	mux := http.NewServeMux()

	// creates GraphQL server with Apollo for performance tracing
	gqlServer := handler.NewDefaultServer(gqlgen.NewExecutableSchema(gqlgen.Config{Resolvers: &gqlgen.Resolver{}}))
	gqlServer.Use(apollotracing.Tracer{})

	// adds handlers for GraphQL and Apollo sandbox environment
	mux.Handle("/api/v1/graphql", gqlServer)
	mux.Handle("/sandbox", playground.ApolloSandboxHandler("Apollo Sandbox", "/api/v1/graphql"))

	// assigns the HTTP server
	g.server = &http.Server{
		Addr:    ":" + g.port,
		Handler: mux,
	}

	return nil
}

func (g *gqlManager) Start() *promise.Promise[any] {
	return promise.New(func(resolve func(any), reject func(error)) {
		log.Printf("GraphQL sandbox available on: http://localhost:%s/sandbox", g.port)

		go func() {
			if err := g.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				reject(err)
			}
		}()

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

	// signals that the server has stopped
	close(g.stop)

	log.Println("GraphQL server shut down successfully")
	return nil
}
