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
	conf         GqlConfig
	schema       graphql.ExecutableSchema
}

// ===== interface assertion =====

var _ a.Plugin = &gqlManager{}

// ===== implementing the a.Plugin interface =====

func New(schema graphql.ExecutableSchema, conf GqlConfig) *gqlManager {
	return &gqlManager{
		conf:   conf,
		schema: schema,
	}
}

func (g *gqlManager) Init() error {
	mux := http.NewServeMux()

	// creates GraphQL server with Apollo
	gqlServer := handler.New(g.schema)
	gqlServer.AddTransport(transport.POST{})
	gqlServer.Use(extension.Introspection{})
	gqlServer.Use(extension.FixedComplexityLimit(g.conf.GetMaxComplexity()))
	// F10: cap total field selections per operation, independent
	// of per-field complexity (cheap fields can otherwise be
	// aliased thousands of times under the complexity ceiling).
	gqlServer.Use(NewAliasLimit(DefaultAliasLimit))

	// OPTIONAL, UNCOMMENT TO ENABLE TRACING
	// gqlServer.Use(apollotracing.Tracer{})

	// adds handlers for GraphQL and Apollo sandbox environment
	mux.Handle("POST /api/v1/graphql", gqlServer)
	mux.Handle("GET /sandbox", pg.ApolloSandboxHandler("Apollo Sandbox", "/api/v1/graphql"))

	// review2 HIGH #18: previously /health (and any non-GraphQL path) fell
	// through to the catch-all and returned SPA HTML, so external probes
	// could not tell "node up" from "serving stale HTML". This is a
	// *liveness* probe only (HTTP server responsive). Readiness/solvency
	// (L2-vs-L1 reconciliation, sync lag) is the separate hardening H1
	// system and is intentionally NOT implemented here — it needs
	// DB/head-height wiring and is consensus-sensitive.
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	c := cors.New(cors.Options{
		AllowedOrigins:   []string{"*"},
		AllowedMethods:   []string{"GET", "POST", "OPTIONS"},
		AllowedHeaders:   []string{"*"},
		AllowCredentials: false,
	})

	// F15: security headers wrap the whole stack so every response
	// (including OPTIONS preflights) gets HSTS / nosniff / etc.
	// assigns the HTTP server
	g.server = &http.Server{
		Addr:              g.conf.GetHostAddr(),
		Handler:           securityHeaders(c.Handler(mux)),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
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
		log.Printf("GraphQL sandbox available on %s/sandbox", g.conf.GetHostAddr())

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
