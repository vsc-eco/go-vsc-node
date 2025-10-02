package gql

import (
	"context"
	"log"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"vsc-node/lib/utils"
	a "vsc-node/modules/aggregate"

	"github.com/gorilla/websocket"

	"github.com/99designs/gqlgen/graphql"
	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/extension"
	"github.com/99designs/gqlgen/graphql/handler/transport"
	pg "github.com/99designs/gqlgen/graphql/playground"
	"github.com/chebyrash/promise"
	"github.com/rs/cors"
)

// ===== constants =====

const shutdownTimeout = 5 * time.Second

// ===== types =====

type gqlManager struct {
	server       *http.Server
	started      atomic.Bool
	startPromise *promise.Promise[any]
	Addr         string
	schema       graphql.ExecutableSchema
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

	// Create GraphQL server with Apollo transports.
	// Note: CheckOrigin is left open (always true) to match the current go-vsc-node setup.
	// Also this enables people to run indexers without any vsc-node
	// It is the responsibility of node operators to secure access (e.g. via nginx/apache),
	// if they don’t want to expose their GraphQL endpoint (including ws://) publicly.
	gqlServer := handler.New(g.schema)
	gqlServer.AddTransport(transport.POST{})
	gqlServer.AddTransport(&transport.Websocket{
		KeepAlivePingInterval: 10 * time.Second,
		Upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},
	})

	gqlServer.Use(extension.Introspection{})

	// adds handlers for GraphQL and Apollo sandbox environment
	mux.Handle("/api/v1/graphql", gqlServer) // accepts querries and subscriptions
	mux.Handle("GET /sandbox", pg.ApolloSandboxHandler("Apollo Sandbox", "/api/v1/graphql"))

	// Configure CORS
	c := cors.New(cors.Options{
		AllowedOrigins:   []string{"*"},
		AllowedMethods:   []string{"GET", "POST", "OPTIONS"},
		AllowedHeaders:   []string{"*"},
		AllowCredentials: false,
	})

	// assigns the HTTP server
	g.server = &http.Server{
		Addr:    g.Addr,
		Handler: c.Handler(mux),
	}

	g.startPromise = promise.New(func(resolve func(any), reject func(error)) {
		g.server.BaseContext = func(l net.Listener) context.Context {
			g.started.Store(true)
			resolve(nil)
			return context.Background()
		}
	})

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

func (g *gqlManager) Started() *promise.Promise[any] {
	if g.started.Load() {
		return utils.PromiseResolve[any](nil)
	}
	return g.startPromise
}
