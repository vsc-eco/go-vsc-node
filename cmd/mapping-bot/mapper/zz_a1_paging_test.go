package mapper

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/hasura/go-graphql-client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A1-A4: fetchStateHex must PAGE a getStateByKeys request so the node (whose resolver
// refuses more than maxStateKeysPerQuery=100 keys) is never sent an over-limit batch. A
// rotation with >100 pending spends / UTXOs would otherwise have the whole request refused,
// so the bot would send no confirmSpend and the rotation driver would go blind past 100.
//
// The test server MIMICS the node's refusal of an over-limit request, so an unpaged
// fetchStateHex fails and only correct paging passes; it also records the largest batch it
// saw and the request count.
func TestA1_FetchStateHex_PagesOverTheNodeKeyLimit(t *testing.T) {
	const total = 250
	state := map[string]string{}
	for i := 0; i < total; i++ {
		state["k"+strconv.Itoa(i)] = hex.EncodeToString([]byte{byte(i)})
	}

	var mu sync.Mutex
	maxPerReq, reqs := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Variables map[string]interface{} `json:"variables"`
		}
		_ = json.Unmarshal(body, &req)
		keys, _ := req.Variables["keys"].([]interface{})

		mu.Lock()
		reqs++
		if len(keys) > maxPerReq {
			maxPerReq = len(keys)
		}
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if len(keys) < 1 || len(keys) > maxStateKeysPerQuery {
			// The node resolver refuses an over-limit request (schema.resolvers.go).
			w.Write([]byte(`{"errors":[{"message":"getStateByKeys: between 1 and 100 keys"}]}`))
			return
		}
		out := map[string]interface{}{}
		for _, k := range keys {
			if v, ok := state[k.(string)]; ok {
				out[k.(string)] = v
			} else {
				out[k.(string)] = nil
			}
		}
		b, _ := json.Marshal(map[string]interface{}{"data": map[string]interface{}{"getStateByKeys": out}})
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	bot := &Bot{GqlClient: graphql.NewClient(srv.URL, srv.Client()), BotConfig: l2TestBotConfig{}, L: slog.Default()}

	keys := make([]string, total)
	for i := range keys {
		keys[i] = "k" + strconv.Itoa(i)
	}
	got, err := bot.fetchStateHex(context.Background(), keys)
	require.NoError(t, err, "an over-limit fetch must be paged, not refused whole")
	assert.Len(t, got, total, "every key's value must come back, merged across pages")

	mu.Lock()
	defer mu.Unlock()
	assert.LessOrEqual(t, maxPerReq, maxStateKeysPerQuery, "no single request may exceed the node's key limit")
	assert.Equal(t, 3, reqs, "250 keys at 100 per page is 3 requests")
}
