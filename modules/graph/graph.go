package graph

import (
	"fmt"
	"log"
	"net/http"
	a "vsc-node/modules/aggregate"

	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/playground"
	"github.com/chebyrash/promise"
)

// type Graph interface{}
type graph struct {
	server *handler.Server
	port   string
	//
	done chan bool
}

var _ a.Plugin = &graph{}

func New() *graph {
	return &graph{
		port: defaultPort,
		done: make(chan bool),
	}
}

const defaultPort = "8080"

func (g *graph) graohQLResolver(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}

	g.server.ServeHTTP(w, r)
}

func (g *graph) playgroundHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	playground.ApolloSandboxHandler("GraphQL playground", "/query").ServeHTTP(w, r)
}

func (g *graph) Init() error {
	g.server = handler.NewDefaultServer(NewExecutableSchema(Config{Resolvers: &Resolver{}}))

	http.HandleFunc("/", g.playgroundHandler)
	http.HandleFunc("/query", g.graohQLResolver)

	return nil
}

func (g *graph) Start() *promise.Promise[any] {
	fmt.Println("Starting the graph plugin")

	go func() {
		//here I am replacing log.Fatal() and using normal error handling method
		log.Printf("connect to http://localhost:%s/ for Graphql playground", g.port)

		if err := http.ListenAndServe(":"+g.port, nil); err != nil && err != http.ErrServerClosed {
			log.Printf("HTTP server error; %v", err)
		}
	}()

	return promise.New(func(resolve func(any), reject func(error)) {
		log.Printf("connect to http://localhost:%s/ for Graphql playground", g.port)
		if err := http.ListenAndServe(":"+g.port, nil); err != nil && err != http.ErrServerClosed {
			reject(err)
			return
		}
		resolve(nil)
	})
}

func (g *graph) Stop() error {
	fmt.Println("Stopping the plugin plugin")
	return nil
}
