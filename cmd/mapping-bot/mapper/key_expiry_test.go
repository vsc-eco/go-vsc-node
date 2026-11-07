package mapper

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"vsc-node/lib/btcvault"

	"github.com/hasura/go-graphql-client"
)

// B8 (EVM bot D7 parity): warn for an expired key and for one expiring within
// keyExpiryWarnEpochs; nothing for a healthy, retired or expiry-less key.
func TestB8_KeyExpiryIssues(t *testing.T) {
	keys := map[string]tssKeyExpiry{
		"c-main":   {status: "active", expiry: 2473},      // 259 epochs left: fine
		"c-mainv1": {status: "active", expiry: 2214 + 28}, // at the warning edge
		"c-mainv2": {status: "deprecated", expiry: 2200},  // expired
		"c-mainv3": {status: "active", expiry: 0},         // no expiry: fine
		"c-mainv4": {status: "deleted", expiry: 100},      // retired: nothing to renew
	}
	got := keyExpiryIssues(keys, 2214)
	if len(got) != 2 {
		t.Fatalf("issues %v, want 2", got)
	}
	if !strings.Contains(got[0], "c-mainv1 expires in 28 epochs (~168h)") || !strings.Contains(got[1], "c-mainv2 has expired") {
		t.Fatalf("issues %v", got)
	}
	if got := keyExpiryIssues(map[string]tssKeyExpiry{"k": {status: "active", expiry: 2214 + 29}}, 2214); got != nil {
		t.Fatalf("29 epochs left warned: %v", got)
	}
	// past the expiry epoch but still "active" (status not flipped yet): warn with 0 left
	if got := keyExpiryIssues(map[string]tssKeyExpiry{"k": {status: "active", expiry: 2200}}, 2214); !reflect.DeepEqual(got, []string{"TSS key k expires in 0 epochs (~0h): run renewKey"}) {
		t.Fatalf("got %v", got)
	}
}

// I-9 wiring: KeyExpiryIssues against a node whose registry has gen 0 purged
// and gen 13 active, with "main" expired long ago and mainv13 healthy: no
// warning (the old list named main and the alarm fired forever).
func TestI9_KeyExpiryIssuesSkipsAPurgedLegacyKey(t *testing.T) {
	reg := append(vaultEntry(0, btcvault.VaultStatusPurged), vaultEntry(13, btcvault.VaultStatusActive)...)
	contractId := l2TestBotConfig{}.ContractId()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Query     string                 `json:"query"`
			Variables map[string]interface{} `json:"variables"`
		}
		_ = json.Unmarshal(body, &req)
		var data any
		switch {
		case strings.Contains(req.Query, "electionByBlockHeight"):
			data = map[string]any{"electionByBlockHeight": map[string]any{"epoch": 2214}}
		case strings.Contains(req.Query, "getTssKey"):
			k := map[string]any{"status": "active", "expiry_epoch": 2473}
			if req.Variables["id"] == contractId+"-main" {
				k = map[string]any{"status": "deprecated", "expiry_epoch": 1500}
			}
			data = map[string]any{"getTssKey": k}
		default: // getStateByKeys
			out := map[string]any{}
			keys, _ := req.Variables["keys"].([]interface{})
			for _, k := range keys {
				out[k.(string)] = nil
				if k == "v" {
					out["v"] = hex.EncodeToString(reg)
				}
			}
			data = map[string]any{"getStateByKeys": out}
		}
		b, _ := json.Marshal(map[string]any{"data": data})
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	bot := &Bot{GqlClient: graphql.NewClient(srv.URL, srv.Client()), GqlURL: srv.URL, BotConfig: l2TestBotConfig{}, L: slog.Default()}
	got, err := bot.KeyExpiryIssues(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("issues %v with gen 0 purged: the legacy main key's expiry is no alarm", got)
	}
}
