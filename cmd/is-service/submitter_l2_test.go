package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A 32-byte secp256k1 private key fixture, hex-encoded (test-only, never
// fund this). Generated via `ethCrypto.GenerateKey`+`hex.EncodeToString`.
const testL2PrivKey = "1010101010101010101010101010101010101010101010101010101010101010"

func TestSubmitterL2_RejectsBadPrivKey(t *testing.T) {
	_, err := NewSubmitterL2(SubmitterL2Config{
		GraphQLEndpoint: "http://x",
		ContractId:      "vsc1foo",
		NetId:           "vsc-testnet",
		PrivateKeyHex:   "not-hex",
	})
	assert.Error(t, err)
}

func TestSubmitterL2_RejectsMissingFields(t *testing.T) {
	cases := []SubmitterL2Config{
		{ContractId: "vsc1foo", NetId: "vsc-testnet", PrivateKeyHex: testL2PrivKey}, // no endpoint
		{GraphQLEndpoint: "http://x", NetId: "vsc-testnet", PrivateKeyHex: testL2PrivKey}, // no contract
		{GraphQLEndpoint: "http://x", ContractId: "vsc1foo", PrivateKeyHex: testL2PrivKey}, // no netId
		{GraphQLEndpoint: "http://x", ContractId: "vsc1foo", NetId: "vsc-testnet"},         // no key
	}
	for _, c := range cases {
		_, err := NewSubmitterL2(c)
		assert.Error(t, err, "config %+v must reject", c)
	}
}

// mockGqlServer returns a fake VSC GraphQL endpoint that answers
// getAccountNonce + submitTransactionV1. Records call counts so tests
// can assert ordering / serialization.
func mockGqlServer(t *testing.T) (*httptest.Server, *struct {
	mu          sync.Mutex
	nonceQueries int
	submits      int
}) {
	t.Helper()
	stats := &struct {
		mu          sync.Mutex
		nonceQueries int
		submits      int
	}{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		require.NoError(t, json.Unmarshal(buf, &req))

		stats.mu.Lock()
		defer stats.mu.Unlock()

		switch {
		case contains(req.Query, "getAccountNonce"):
			stats.nonceQueries++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"getAccountNonce": map[string]any{"nonce": uint64(42)},
				},
			})
		case contains(req.Query, "submitTransactionV1"):
			stats.submits++
			id := "bafyfake"
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"submitTransactionV1": map[string]any{"id": id},
				},
			})
		default:
			http.Error(w, "unknown query", http.StatusBadRequest)
		}
	}))
	return srv, stats
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestSubmitterL2_HappyPath(t *testing.T) {
	srv, stats := mockGqlServer(t)
	defer srv.Close()

	sub, err := NewSubmitterL2(SubmitterL2Config{
		GraphQLEndpoint: srv.URL,
		ContractId:      "vsc1mapper",
		NetId:           "vsc-testnet",
		RcLimit:         1000,
		PrivateKeyHex:   testL2PrivKey,
	})
	require.NoError(t, err)

	payload := MapInstantSendPayload{
		Body: MapInstantSendBody{
			RawTxHex:    "deadbeef",
			Instruction: "op=auth;sid=test",
			Epoch:       1,
			ChainId:     "vsc-testnet",
		},
		Agg: MapInstantSendAgg{AggSigHex: "aa"},
	}

	l2TxID, err := sub.SubmitMapInstantSend(context.Background(), payload)
	require.NoError(t, err)
	assert.Equal(t, "bafyfake", l2TxID, "L2 txID must be returned, not discarded")

	stats.mu.Lock()
	defer stats.mu.Unlock()
	assert.Equal(t, 1, stats.nonceQueries, "should fetch nonce once")
	assert.Equal(t, 1, stats.submits, "should submit once")
}

func TestSubmitterL2_GraphqlErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"errors": []map[string]any{{"message": "nonce service down"}},
		})
	}))
	defer srv.Close()

	sub, err := NewSubmitterL2(SubmitterL2Config{
		GraphQLEndpoint: srv.URL,
		ContractId:      "vsc1mapper",
		NetId:           "vsc-testnet",
		PrivateKeyHex:   testL2PrivKey,
	})
	require.NoError(t, err)

	_, err = sub.SubmitMapInstantSend(context.Background(), MapInstantSendPayload{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nonce service down")
}

func TestSubmitterL2_ContextCancelDuringLock(t *testing.T) {
	srv, _ := mockGqlServer(t)
	defer srv.Close()
	sub, err := NewSubmitterL2(SubmitterL2Config{
		GraphQLEndpoint: srv.URL, ContractId: "x", NetId: "vsc-testnet",
		PrivateKeyHex: testL2PrivKey,
	})
	require.NoError(t, err)

	// Take the lock first, then call with a cancelled context.
	require.NoError(t, sub.submitMu.lock(context.Background()))
	defer sub.submitMu.unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = sub.SubmitMapInstantSend(ctx, MapInstantSendPayload{})
	assert.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}
