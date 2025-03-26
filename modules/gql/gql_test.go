package gql_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
	"vsc-node/lib/test_utils"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"
	"vsc-node/modules/db/vsc/witnesses"
	"vsc-node/modules/gql"
	"vsc-node/modules/gql/gqlgen"

	"github.com/stretchr/testify/assert"
)

func TestQueryAndMutation(t *testing.T) {
	// init the gql plugin with an in-memory test server
	dbConfg := db.NewDbConfig()
	d := db.New(dbConfg)
	vscDb := vsc.New(d)
	witnesses := witnesses.New(vscDb)
	resolver := &gqlgen.Resolver{
		witnesses,
	}
	schema := gqlgen.NewExecutableSchema(gqlgen.Config{Resolvers: resolver})

	g := gql.New(schema, "localhost:8081")
	agg := aggregate.New([]aggregate.Plugin{
		dbConfg,
		d,
		vscDb,
		witnesses,
		g,
	})
	test_utils.RunPlugin(t, agg)

	ctx, _ := context.WithTimeout(context.Background(), 500*time.Millisecond)
	_, err := g.Started().Await(ctx)
	assert.NoError(t, err)

	// test get current number query
	query := `{"query": "query { witnessNodes(height: 52) { net_id } }"}`
	resp := performGraphQLRequest(t, "http://"+g.Addr+"/api/v1/graphql", query)

	expectedQuery := map[string]interface{}{
		"data": map[string]interface{}{
			"getCurrentNumber": map[string]interface{}{
				"currentNumber": float64(1),
			},
		},
	}
	assert.Equal(t, expectedQuery, resp)
}

// ===== test helpers =====

func performGraphQLRequest(t *testing.T, url, query string) map[string]interface{} {
	req, err := http.NewRequest("POST", url, bytes.NewBufferString(query))
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("failed to send request: %v", err)
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	return result
}
