package gql_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
	"vsc-node/lib/datalayer"
	"vsc-node/lib/logger"
	"vsc-node/lib/test_utils"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/hive_blocks"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	"vsc-node/modules/db/vsc/nonces"
	rcDb "vsc-node/modules/db/vsc/rcs"
	"vsc-node/modules/db/vsc/transactions"
	tss_db "vsc-node/modules/db/vsc/tss"
	vscBlocks "vsc-node/modules/db/vsc/vsc_blocks"
	"vsc-node/modules/db/vsc/witnesses"
	"vsc-node/modules/gql"
	"vsc-node/modules/gql/gqlgen"
	libp2p "vsc-node/modules/p2p"
	stateEngine "vsc-node/modules/state-processing"
	transactionpool "vsc-node/modules/transaction-pool"
	wasm_runtime "vsc-node/modules/wasm/runtime_ipc"

	"github.com/stretchr/testify/assert"
)

func TestQueryAndMutation(t *testing.T) {
	// init the gql plugin with an in-memory test server
	l := logger.PrefixedLogger{
		Prefix: "vsc-node",
	}
	sysConfig := systemconfig.MocknetConfig()
	dbConfg := db.NewDbConfig()
	identityConfig := common.NewIdentityConfig()
	d := db.New(dbConfg)
	vscDb := vsc.New(d)
	witnesses := witnesses.New(vscDb)
	p2p := libp2p.New(witnesses, identityConfig, sysConfig, nil)
	hiveBlocks, hiveBlocksErr := hive_blocks.New(vscDb)
	electionDb := elections.New(vscDb)
	contractDb := contracts.New(vscDb)
	txDb := transactions.New(vscDb)
	vscBlocks := vscBlocks.New(vscDb)
	ledgerDbImpl := ledgerDb.New(vscDb)
	balanceDb := ledgerDb.NewBalances(vscDb)
	actionsDb := ledgerDb.NewActionsDb(vscDb)
	interestClaims := ledgerDb.NewInterestClaimDb(vscDb)
	contractState := contracts.NewContractState(vscDb)
	nonceDb := nonces.New(vscDb)
	rcDb := rcDb.New(vscDb)
	tssKeys := tss_db.NewKeys(vscDb)
	tssCommitments := tss_db.NewCommitments(vscDb)
	tssRequests := tss_db.NewRequests(vscDb)
	da := datalayer.New(p2p)
	conf := common.NewIdentityConfig()
	balances := ledgerDb.NewBalances(vscDb)
	wasm := wasm_runtime.New()

	assert.NoError(t, hiveBlocksErr)
	se := stateEngine.New(l, sysConfig, da, witnesses, electionDb, contractDb, contractState, txDb, ledgerDbImpl, balanceDb, hiveBlocks, interestClaims, vscBlocks, actionsDb, rcDb, nonceDb, tssKeys, tssCommitments, tssRequests, wasm)
	txPool := transactionpool.New(p2p, txDb, nonceDb, electionDb, hiveBlocks, da, conf, se.RcSystem)
	resolver := &gqlgen.Resolver{
		witnesses,
		txPool,
		balances,
		ledgerDbImpl,
		actionsDb,
		electionDb,
		txDb,
		nonceDb,
		rcDb,
		hiveBlocks,
		se,
		da,
		contractDb,
		contractState,
		tssKeys,
		tssRequests,
	}
	schema := gqlgen.NewExecutableSchema(gqlgen.Config{Resolvers: resolver})

	g := gql.New(schema, "localhost:8081")
	agg := aggregate.New([]aggregate.Plugin{
		dbConfg,
		d,
		vscDb,
		witnesses,
		p2p,
		txDb,
		da,
		conf,
		txPool,
		balances,
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
