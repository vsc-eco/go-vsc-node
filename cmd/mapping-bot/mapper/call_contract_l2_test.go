package mapper

import (
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

	"bytes"
)

// l2TestBotConfig uses a VSC-prefixed contract id — the transactionpool
// validator rejects anything else. testBotConfig returns "test-contract"
// which would fail that validation.
type l2TestBotConfig struct{}

func (l2TestBotConfig) ContractId() string { return "vsc1BkWohDf5fPcwn7V9B9ar6TyiWc3A2ZGJ4t" }
func (l2TestBotConfig) HttpPort() uint16   { return 0 }
func (l2TestBotConfig) SignApiKey() string { return "" }
func (l2TestBotConfig) OpsApiKey() string  { return "" }
func (l2TestBotConfig) FilePath() string   { return "" }
func (l2TestBotConfig) RcLimit() uint      { return 10000 }

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

// TestCallContractL2_TooLarge ensures transactions that would exceed the L2
// size limit are rejected with an error (not silently dropped).
func TestCallContractL2_TooLarge(t *testing.T) {
	gql := &mockGraphQL{nonces: map[string]uint64{}}
	bot, did := buildBotForL2Test(t, gql)
	gql.nonces[did.String()] = 0

	// Build a payload large enough that the serialized CBOR will exceed transactionpool.MAX_TX_SIZE.
	// The CBOR overhead is small; a ~16000-byte payload string will push it over.
	hugePayload := json.RawMessage(`"` + strings.Repeat("x", 16000) + `"`)

	_, err := bot.callContractL2(context.Background(), hugePayload, "map")
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("want too-large error, got %v", err)
	}
	// SubmitTransactionV1 must NOT have been called.
	if len(gql.submitted) != 0 {
		t.Errorf("expected no submissions for oversized tx, got %d", len(gql.submitted))
	}
}
