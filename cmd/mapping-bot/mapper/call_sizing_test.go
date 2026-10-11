package mapper

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	"vsc-node/modules/common"
	transactionpool "vsc-node/modules/transaction-pool"
)

// BOT-CONF-RC (testnet 2026-10-11): confirmSpend for the 17-input gen-1 sweep
// c5adcae8 needs 16,752 RC (dry run), but the bot sent it with the flat configured
// rc_limit of 10,000, so every attempt FAILED on chain and burned the bot's RC
// (118,374 -> 1,215) while the sweep could not settle. map has the same flat
// limit, and a map that fails on chain is final, so its deposit goes uncredited.
// The non-vault calls must be sized from their dry run, never below the
// configured limit.
func TestCallContractL2_NonVaultCallsSizedByDryRun(t *testing.T) {
	submittedRcLimit := func(t *testing.T, sim *SimulatedCall, action string) uint64 {
		t.Helper()
		gql := &mockGraphQL{nonces: map[string]uint64{}, sim: sim}
		bot, did := buildBotForL2Test(t, gql)
		gql.accountRC = map[string]int64{did.String(): 468139} // mainnet bot DID, 2026-10-11
		if _, err := bot.callContractL2(context.Background(), json.RawMessage(`{"tx_data":{}}`), action); err != nil {
			t.Fatalf("%s: callContractL2: %v", action, err)
		}
		if len(gql.submitted) != 1 {
			t.Fatalf("%s: want 1 submitted tx, got %d", action, len(gql.submitted))
		}
		raw, err := base64.URLEncoding.DecodeString(gql.submitted[0].TxB64)
		if err != nil {
			t.Fatalf("decode tx b64: %v", err)
		}
		var shell transactionpool.VSCTransactionShell
		if err := common.DecodeCbor(raw, &shell); err != nil {
			t.Fatalf("decode cbor: %v", err)
		}
		return shell.Headers.RcLimit
	}

	cases := []struct {
		name   string
		action string
		sim    *SimulatedCall
		want   uint64
	}{
		{"17-input sweep confirmSpend gets room for its real cost", "confirmSpend", &SimulatedCall{Success: true, RcUsed: 16752}, 33504},
		{"a costly map is sized the same way", "map", &SimulatedCall{Success: true, RcUsed: 12000}, 24000},
		{"a cheap call keeps the configured limit", "confirmSpend", &SimulatedCall{Success: true, RcUsed: 3000}, 10000},
		{"no dry run available: configured limit, as before", "confirmSpend", nil, 10000},
		{"dry run refused: configured limit, the node judges the call", "map", &SimulatedCall{Success: false, Err: "bad_input"}, 10000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := submittedRcLimit(t, c.sim, c.action); got != c.want {
				t.Fatalf("%s rc_limit = %d, want %d", c.action, got, c.want)
			}
		})
	}
}
