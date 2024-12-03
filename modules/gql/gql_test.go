package gql_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"vsc-node/lib/test_utils"
	"vsc-node/modules/gql"
	"vsc-node/modules/gql/gqlgen"

	"github.com/stretchr/testify/assert"
)

// ===== mock resolver =====

type MockResolver struct {
	StateCounter int
}

func (r *MockResolver) Mutation() gqlgen.MutationResolver {
	return &mockMutationResolver{r}
}

func (r *MockResolver) Query() gqlgen.QueryResolver {
	return &mockQueryResolver{r}
}

type mockQueryResolver struct {
	*MockResolver
}

func (q *mockQueryResolver) GetCurrentNumber(ctx context.Context) (*gqlgen.TestResult, error) {
	return &gqlgen.TestResult{
		CurrentNumber: &q.StateCounter,
	}, nil
}

// stub implementations of non-needed methods
func (q *mockQueryResolver) ActiveWitnessNodes(ctx context.Context) (*string, error) {
	panic("not implemented")
}

func (q *mockQueryResolver) AnchorProducer(ctx context.Context) (*gqlgen.AnchorProducer, error) {
	panic("not implemented")
}

func (q *mockQueryResolver) ContractStateDiff(ctx context.Context, id *string) (*gqlgen.ContractDiff, error) {
	panic("not implemented")
}

func (q *mockQueryResolver) ContractState(ctx context.Context, id *string) (*gqlgen.ContractState, error) {
	panic("not implemented")
}

func (q *mockQueryResolver) FindTransaction(ctx context.Context, filterOptions *gqlgen.FindTransactionFilter, decodedFilter *string) (*gqlgen.FindTransactionResult, error) {
	panic("not implemented")
}

func (q *mockQueryResolver) FindContractOutput(ctx context.Context, filterOptions *gqlgen.FindContractOutputFilter, decodedFilter *string) (*gqlgen.FindContractOutputResult, error) {
	panic("not implemented")
}

func (q *mockQueryResolver) FindLedgerTXs(ctx context.Context, filterOptions *gqlgen.LedgerTxFilter) (*gqlgen.LedgerResults, error) {
	panic("not implemented")
}

func (q *mockQueryResolver) GetAccountBalance(ctx context.Context, account *string) (*gqlgen.GetBalanceResult, error) {
	panic("not implemented")
}

func (q *mockQueryResolver) FindContract(ctx context.Context, id *string) (*gqlgen.FindContractResult, error) {
	panic("not implemented")
}

func (q *mockQueryResolver) SubmitTransactionV1(ctx context.Context, tx string, sig string) (*gqlgen.TransactionSubmitResult, error) {
	panic("not implemented")
}

func (q *mockQueryResolver) GetAccountNonce(ctx context.Context, keyGroup []*string) (*gqlgen.AccountNonceResult, error) {
	panic("not implemented")
}

func (q *mockQueryResolver) LocalNodeInfo(ctx context.Context) (*gqlgen.LocalNodeInfo, error) {
	panic("not implemented")
}

func (q *mockQueryResolver) WitnessNodes(ctx context.Context, height *int) ([]*gqlgen.WitnessNode, error) {
	panic("not implemented")
}

func (q *mockQueryResolver) WitnessSchedule(ctx context.Context, height *int) (*string, error) {
	panic("not implemented")
}

func (q *mockQueryResolver) NextWitnessSlot(ctx context.Context, self *bool) (*string, error) {
	panic("not implemented")
}

func (q *mockQueryResolver) WitnessActiveScore(ctx context.Context, height *int) (*string, error) {
	panic("not implemented")
}

func (q *mockQueryResolver) MockGenerateElection(ctx context.Context) (*string, error) {
	panic("not implemented")
}

// ===== mock mutation resolver =====

type mockMutationResolver struct {
	*MockResolver
}

func (m *mockMutationResolver) IncrementNumber(ctx context.Context) (*gqlgen.TestResult, error) {
	m.StateCounter++
	return &gqlgen.TestResult{
		CurrentNumber: &m.StateCounter, // inc val
	}, nil
}

// ===== tests =====

func TestQueryAndMutation(t *testing.T) {
	// init the gql plugin with an in-memory test server
	resolver := &MockResolver{StateCounter: 0}
	schema := gqlgen.NewExecutableSchema(gqlgen.Config{Resolvers: resolver})

	g := gql.New(schema, "localhost:8080")
	test_utils.RunPlugin(t, g)

	// test inc number mutation
	mutation := `{"query": "mutation { incrementNumber { currentNumber } }"}`
	resp := performGraphQLRequest(t, "http://"+g.Addr+"/api/v1/graphql", mutation)

	expectedMutation := map[string]interface{}{
		"data": map[string]interface{}{
			"incrementNumber": map[string]interface{}{
				"currentNumber": float64(1),
			},
		},
	}
	assert.Equal(t, expectedMutation, resp)

	// test get current number query
	query := `{"query": "query { getCurrentNumber { currentNumber } }"}`
	resp = performGraphQLRequest(t, "http://"+g.Addr+"/api/v1/graphql", query)

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
