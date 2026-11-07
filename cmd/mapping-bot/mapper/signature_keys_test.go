package mapper

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"vsc-node/lib/btcvault"

	"github.com/hasura/go-graphql-client"
)

// vaultEntry packs one 91-byte vault registry entry (the contract's "v" layout).
func vaultEntry(gen uint32, status btcvault.VaultStatus) []byte {
	e := make([]byte, btcvault.VaultEntrySize)
	binary.BigEndian.PutUint32(e[0:], gen)
	e[4] = 0x02
	e[37] = 0x03
	e[70] = byte(status)
	return e
}

func TestSignatureKeyIds(t *testing.T) {
	const c = "vsc1Contract"
	cases := []struct {
		name   string
		vaults []btcvault.Vault
		want   []string
	}{
		{"no registry (non-vault contract, pre-fold vault)", nil, []string{c + "-main"}},
		{"gen 0 draining, gen 1 active", []btcvault.Vault{
			{Generation: 0, Status: btcvault.VaultStatusDraining},
			{Generation: 1, Status: btcvault.VaultStatusActive},
		}, []string{c + "-main", c + "-mainv1"}},
		{"purged generation is not queried, main always is", []btcvault.Vault{
			{Generation: 0, Status: btcvault.VaultStatusPurged},
			{Generation: 1, Status: btcvault.VaultStatusRetiring},
			{Generation: 2, Status: btcvault.VaultStatusActive},
			{Generation: 3, Status: btcvault.VaultStatusPending},
		}, []string{c + "-main", c + "-mainv1", c + "-mainv2", c + "-mainv3"}},
	}
	for _, tc := range cases {
		if got := signatureKeyIds(c, tc.vaults); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

// fakeSigNode answers the two queries FetchSignatures makes: the vault registry
// read and getTssRequests per key id. Every sign request is complete and lives
// under exactly one key, as on a real node.
type fakeSigNode struct {
	mu         sync.Mutex
	registry   []byte                       // "v" bytes; nil = no registry
	sigsByKey  map[string]map[string]string // keyId -> msg -> sig hex
	stateFails bool
	keysAsked  []string
}

func (f *fakeSigNode) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Query     string                 `json:"query"`
			Variables map[string]interface{} `json:"variables"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("bad request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case strings.Contains(req.Query, "getStateByKeys"):
			if f.stateFails {
				w.Write([]byte(`{"errors":[{"message":"node unavailable"}],"data":null}`))
				return
			}
			st := map[string]interface{}{"v": nil}
			if f.registry != nil {
				st["v"] = hex.EncodeToString(f.registry)
			}
			out, _ := json.Marshal(map[string]interface{}{"data": map[string]interface{}{"getStateByKeys": st}})
			w.Write(out)
		case strings.Contains(req.Query, "getTssRequests"):
			keyId, _ := req.Variables["keyId"].(string)
			f.keysAsked = append(f.keysAsked, keyId)
			rows := []map[string]string{}
			msgs, _ := req.Variables["msgHex"].([]interface{})
			for _, m := range msgs {
				if sig, ok := f.sigsByKey[keyId][m.(string)]; ok {
					rows = append(rows, map[string]string{"msg": m.(string), "sig": sig, "status": "complete"})
				}
			}
			out, _ := json.Marshal(map[string]interface{}{"data": map[string]interface{}{"getTssRequests": rows}})
			w.Write(out)
		default:
			t.Errorf("unexpected query: %s", req.Query)
		}
	})
}

func newSigTestBot(t *testing.T, f *fakeSigNode) *Bot {
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	return &Bot{
		GqlClient: graphql.NewClient(srv.URL, srv.Client()),
		BotConfig: l2TestBotConfig{},
		L:         slog.Default(),
	}
}

// BOT-KEYGEN-1 (testnet 2026-10-06): an unmap is signed by the ACTIVE generation
// and a migration sweep by the RETIRING one. With gen 0 retiring and gen 1 active
// the bot must collect both, not only the legacy "main" key's.
func TestFetchSignatures_CollectsEveryLiveGeneration(t *testing.T) {
	const c = "vsc1BkWohDf5fPcwn7V9B9ar6TyiWc3A2ZGJ4t"
	sweepMsg, unmapMsg := strings.Repeat("aa", 32), strings.Repeat("bb", 32)
	f := &fakeSigNode{
		registry: append(vaultEntry(0, btcvault.VaultStatusDraining), vaultEntry(1, btcvault.VaultStatusActive)...),
		sigsByKey: map[string]map[string]string{
			c + "-main":   {sweepMsg: "3044" + strings.Repeat("01", 66)},
			c + "-mainv1": {unmapMsg: "3044" + strings.Repeat("02", 66)},
		},
	}
	bot := newSigTestBot(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	got, err := bot.FetchSignatures(ctx, []string{sweepMsg, unmapMsg})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got[sweepMsg]; !ok {
		t.Error("the retiring generation's (main) sweep signature was not returned")
	}
	if _, ok := got[unmapMsg]; !ok {
		t.Error("the active generation's (mainv1) unmap signature was not returned: the BOT-KEYGEN-1 stall")
	}
	if !reflect.DeepEqual(f.keysAsked, []string{c + "-main", c + "-mainv1"}) {
		t.Errorf("key ids queried: %v", f.keysAsked)
	}
}

// A registry the bot cannot read must fail the cycle (it retries next block),
// never quietly fall back to "main" only, which is the stall being fixed.
func TestFetchSignatures_UnreadableRegistryFailsTheCycle(t *testing.T) {
	f := &fakeSigNode{stateFails: true}
	bot := newSigTestBot(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := bot.FetchSignatures(ctx, []string{strings.Repeat("aa", 32)}); err == nil {
		t.Fatal("expected an error when the vault registry cannot be read")
	}
	if len(f.keysAsked) != 0 {
		t.Errorf("no sign request may be queried without the key set; asked %v", f.keysAsked)
	}
}

// A contract with no registry (dash/ltc mappings, a pre-fold BTC vault) keeps
// the old behaviour exactly: one query, the "main" key.
func TestFetchSignatures_NoRegistryQueriesMainOnly(t *testing.T) {
	const c = "vsc1BkWohDf5fPcwn7V9B9ar6TyiWc3A2ZGJ4t"
	msg := strings.Repeat("cc", 32)
	f := &fakeSigNode{sigsByKey: map[string]map[string]string{c + "-main": {msg: "3044" + strings.Repeat("03", 66)}}}
	bot := newSigTestBot(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got, err := bot.FetchSignatures(ctx, []string{msg})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got[msg]; !ok || !reflect.DeepEqual(f.keysAsked, []string{c + "-main"}) {
		t.Fatalf("got %v, asked %v", got, f.keysAsked)
	}
}
