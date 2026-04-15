package mapper

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"vsc-node/lib/dids"
	"vsc-node/modules/common"
	systemconfig "vsc-node/modules/common/system-config"
	transactionpool "vsc-node/modules/transaction-pool"

	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

// TestEstimateHiveEnvelopeSize covers the size-estimation math that drives the
// auto-routing decision. The estimate should scale with payload length and
// cross the hiveEnvelopeThreshold at the expected point.
func TestEstimateHiveEnvelopeSize(t *testing.T) {
	mkPayload := func(n int) json.RawMessage {
		return json.RawMessage(bytes.Repeat([]byte("a"), n))
	}

	tests := []struct {
		name              string
		payloadLen        int
		wantOverThreshold bool
		wantOverHiveCap   bool
	}{
		{"small typical map", 1369, false, false},
		{"mid-range", 5000, false, false},
		{"just below threshold", 7000, false, false},
		{"at threshold", 7500, true, false},
		{"failing bc1q74 tx", 8687, true, true}, // actual failing tx's payload_len
		{"over cap", 12000, true, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := estimateHiveEnvelopeSize(
				"map",
				"vsc-mainnet",
				"milo.vsc",
				"vsc1BdrQ6EtbQ64rq2PkPd21x4MaLnVRcJj85d",
				mkPayload(tt.payloadLen),
			)
			overThreshold := got > hiveEnvelopeThreshold
			overCap := got > hiveCustomJsonMaxSize
			if overThreshold != tt.wantOverThreshold {
				t.Errorf("payload=%d envelope=%d: overThreshold=%v want %v",
					tt.payloadLen, got, overThreshold, tt.wantOverThreshold)
			}
			if overCap != tt.wantOverHiveCap {
				t.Errorf("payload=%d envelope=%d: overHiveCap=%v want %v",
					tt.payloadLen, got, overCap, tt.wantOverHiveCap)
			}
		})
	}
}

// l2TestBotConfig uses a VSC-prefixed contract id — the transactionpool
// validator rejects anything else. testBotConfig returns "test-contract"
// which would fail that validation.
type l2TestBotConfig struct{}

func (l2TestBotConfig) ContractId() string { return "vsc1BkWohDf5fPcwn7V9B9ar6TyiWc3A2ZGJ4t" }
func (l2TestBotConfig) HttpPort() uint16   { return 0 }
func (l2TestBotConfig) SignApiKey() string { return "" }
func (l2TestBotConfig) FilePath() string   { return "" }

// buildBotForL2Test creates a minimal Bot wired for L2-path tests.
func buildBotForL2Test(t *testing.T, gql GraphQLFetcher) (*Bot, dids.EthDID) {
	t.Helper()
	priv, err := ethCrypto.ToECDSA(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatalf("ToECDSA: %v", err)
	}
	did := dids.NewEthDID(ethCrypto.PubkeyToAddress(priv.PublicKey).Hex())
	return &Bot{
		L:            slog.Default(),
		BotConfig:    l2TestBotConfig{},
		SystemConfig: systemconfig.TestnetConfig(),
		Gql:          gql,
		botEthKey:    priv,
		botEthDID:    did,
	}, did
}

// TestCallContractL2_MissingKey ensures a clear error surfaces when the bot
// has no L2 signing key provisioned.
func TestCallContractL2_MissingKey(t *testing.T) {
	bot, _ := buildBotForL2Test(t, &mockGraphQL{})
	bot.botEthKey = nil

	_, err := bot.callContractL2(context.Background(), json.RawMessage(`{}`), "map")
	if err == nil || !strings.Contains(err.Error(), "L2 signing key not configured") {
		t.Fatalf("want missing-key error, got %v", err)
	}
}

// TestCallContractL2_SubmitsSignedTx asserts the L2 path:
//   - fetches the account nonce using the bot's DID
//   - builds a VscContractCall with caller=bot DID, action, payload
//   - signs via TransactionCrafter + EthProvider
//   - invokes SubmitTransactionV1 with base64url(tx)+base64url(sig)
//   - returns the node-assigned id
//
// It decodes the submitted CBOR to confirm caller/nonce/action landed through.
func TestCallContractL2_SubmitsSignedTx(t *testing.T) {
	gql := &mockGraphQL{nonces: map[string]uint64{}}
	bot, did := buildBotForL2Test(t, gql)
	gql.nonces[did.String()] = 17

	payload := json.RawMessage(`{"tx_data":{"block_height":1,"raw_tx_hex":"ab","merkle_proof_hex":"cd","tx_index":0},"instructions":["to=alice"]}`)

	txID, err := bot.callContractL2(context.Background(), payload, "map")
	if err != nil {
		t.Fatalf("callContractL2 error: %v", err)
	}
	if !strings.HasPrefix(txID, "bafyrei-mock-l2-") {
		t.Fatalf("unexpected tx id %q", txID)
	}
	if len(gql.submitted) != 1 {
		t.Fatalf("want 1 submitted tx, got %d", len(gql.submitted))
	}

	sub := gql.submitted[0]
	rawTx, err := base64.URLEncoding.DecodeString(sub.TxB64)
	if err != nil {
		t.Fatalf("decode tx b64: %v", err)
	}
	var shell transactionpool.VSCTransactionShell
	if err := common.DecodeCbor(rawTx, &shell); err != nil {
		t.Fatalf("decode cbor: %v", err)
	}
	if shell.Headers.Nonce != 17 {
		t.Errorf("nonce: got %d want 17", shell.Headers.Nonce)
	}
	if shell.Headers.NetId != "vsc-testnet" {
		t.Errorf("net_id: got %q want vsc-testnet", shell.Headers.NetId)
	}
	if len(shell.Headers.RequiredAuths) != 1 || shell.Headers.RequiredAuths[0] != did.String() {
		t.Errorf("required_auths: got %v want [%s]", shell.Headers.RequiredAuths, did.String())
	}
	if len(shell.Tx) != 1 || shell.Tx[0].Type != "call" {
		t.Errorf("tx op shape: %+v", shell.Tx)
	}
}

// TestCallContractL2_SubmitErrorSurfaces ensures submit failures are
// propagated so callWithRetry will retry rather than silently succeed.
func TestCallContractL2_SubmitErrorSurfaces(t *testing.T) {
	gql := &mockGraphQL{submitErr: errBotEthKeyMissing}
	bot, _ := buildBotForL2Test(t, gql)

	_, err := bot.callContractL2(context.Background(), json.RawMessage(`{}`), "map")
	if err == nil {
		t.Fatal("expected SubmitTransactionV1 error to surface")
	}
}

// TestCallContract_RoutesToL2_OnLargePayload asserts the auto-routing wiring
// in callContract: large payloads bypass the Hive path and go to L2.
//
// Since callContract builds a real Hive RPC client even for the Hive branch,
// this test supplies only the L2 prerequisites and confirms the L2 mock was
// invoked without ever touching Hive. A small payload is NOT covered here —
// that path requires a live Hive endpoint and is exercised separately.
func TestCallContract_RoutesToL2_OnLargePayload(t *testing.T) {
	gql := &mockGraphQL{nonces: map[string]uint64{}}
	bot, did := buildBotForL2Test(t, gql)
	gql.nonces[did.String()] = 0

	// Need hiveUsername for the size estimator. Inject via identityConfig.
	bot.IdentityConfig = common.NewIdentityConfig()

	// Oversized payload → should route to L2.
	bigPayload := json.RawMessage(bytes.Repeat([]byte("a"), 9000))
	id, err := bot.callContract(context.Background(), bigPayload, "map")
	if err != nil {
		t.Fatalf("callContract L2 route: %v", err)
	}
	if !strings.HasPrefix(id, "bafyrei-mock-l2-") {
		t.Errorf("expected L2 id prefix, got %q", id)
	}
	if len(gql.submitted) != 1 {
		t.Errorf("expected 1 L2 submission, got %d", len(gql.submitted))
	}
}
