package mapper

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"vsc-node/cmd/mapping-bot/chain"
	"vsc-node/cmd/mapping-bot/database"
	"vsc-node/lib/btcvault"

	contractinterface "vsc-node/cmd/mapping-bot/contract-interface"

	"github.com/hasura/go-graphql-client"
)

// BOT-REDRIVE-2 (testnet 2026-10-11, shared vault, gen 1 -> gen 2): the 17-input
// sweep 067a038a took ~49 min to collect its TSS signatures, so the stale window
// (counted from when the bot first saw it in flight) had run out before it was ever
// broadcast; the bot re-drove it 13 s after posting it (c5adcae8, +2,806 sats from
// the fee reserve) and 3 min later tried to re-drive the replaced original AGAIN.
// The contract accepts that (the original's own build height stays old) and
// out-bids the group's highest fee each time, so only the RC pre-flight stopped a
// fee escalation up to the 64-member cap. The driver must start the clock only once
// it holds the sweep fully signed, and only ever re-drive the group's newest member.
func TestHandleVaultRotation_RedrivesOnlyTheNewestSignedSweep(t *testing.T) {
	const (
		orig = "067a038a672996f5b608ea5d7125588ef97c48cffe7a7531f9d583266d5ab2a4"
		repl = "c5adcae8d2bceca2f4c591499e86b4d5d5af42ea153e501b180297ed6e5f5f0d"
	)
	resetDriverGlobals := func() {
		sweepSeenMu.Lock()
		firstSeenSweep = map[string]uint64{}
		sweepSeenMu.Unlock()
		vaultOpMu.Lock()
		delete(lastVaultOpAt, l2TestBotConfig{}.ContractId())
		vaultOpMu.Unlock()
	}
	resetDriverGlobals()
	t.Cleanup(resetDriverGlobals)

	// Contract state: gen 1 retiring with one coin left (so the driver waits on the
	// sweep instead of retiring), gen 2 active, fee reserve funded, orig in flight.
	var stMu sync.Mutex
	utxo := make([]byte, 32+4+8)
	utxo = append(utxo, 0, 0) // empty pkscript, empty tag
	utxo = binary.BigEndian.AppendUint32(utxo, 1)
	supply := make([]byte, 32)
	binary.BigEndian.PutUint64(supply[16:], 21950)
	state := map[string]string{
		contractinterface.VaultRegistryKey: hex.EncodeToString(append(
			vaultEntry(1, btcvault.VaultStatusRetiring), vaultEntry(2, btcvault.VaultStatusActive)...)),
		contractinterface.UtxoRegistryKey:             "0007000000000000",
		contractinterface.UtxoPrefix + "7":            hex.EncodeToString(utxo),
		contractinterface.SupplyKey:                   hex.EncodeToString(supply),
		contractinterface.MigrationSweepPrefix + orig: sweepRecordHex(7),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Variables map[string]interface{} `json:"variables"`
		}
		_ = json.Unmarshal(body, &req)
		out := map[string]interface{}{}
		keys, _ := req.Variables["keys"].([]interface{})
		stMu.Lock()
		for _, k := range keys {
			if v, ok := state[k.(string)]; ok {
				out[k.(string)] = v
			} else {
				out[k.(string)] = nil
			}
		}
		stMu.Unlock()
		b, _ := json.Marshal(map[string]interface{}{"data": map[string]interface{}{"getStateByKeys": out}})
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
	}))
	t.Cleanup(srv.Close)

	gql := &mockGraphQL{
		txSpends:   map[string]*contractinterface.SigningData{orig: {}},
		txStatuses: map[string]string{"mock-tx-id": "CONFIRMED"},
	}
	caller := &mockContractCaller{}
	btc := newMockChainClient()
	db := newMockStateStore()
	db.txs[orig] = &database.Transaction{TxID: orig, State: database.TxStatePending, TotalSignatures: 17, CurrentSignatures: 3}
	bot := &Bot{
		L:         slog.Default(),
		BotConfig: l2TestBotConfig{},
		Gql:       gql,
		GqlClient: graphql.NewClient(srv.URL, srv.Client()),
		Caller:    caller,
		StateDB:   db,
		Chain:     &chain.ChainConfig{Client: btc},
	}

	redriven := func() []string {
		var ids []string
		for _, c := range caller.getCalls() {
			if c.Action == "redriveSpend" {
				ids = append(ids, string(c.Payload))
			}
		}
		return ids
	}
	cycle := func(height uint64, want []string, why string) {
		t.Helper()
		gql.mu.Lock()
		gql.lastHeight = strconv.FormatUint(height, 10)
		gql.mu.Unlock()
		vaultOpMu.Lock()
		delete(lastVaultOpAt, l2TestBotConfig{}.ContractId()) // minutes pass between cycles
		vaultOpMu.Unlock()
		bot.HandleVaultRotation()
		got := redriven()
		if len(got) != len(want) {
			t.Fatalf("h=%d: re-drives %v, want %v (%s)", height, short(got), short(want), why)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("h=%d: re-drives %v, want %v (%s)", height, short(got), short(want), why)
			}
		}
	}

	cycle(5158262, nil, "the sweep was just built")
	cycle(5158276, nil, "14 blocks in flight but still collecting signatures: nothing to re-drive yet")

	db.txs[orig].CurrentSignatures = 17
	db.txs[orig].State = database.TxStateSent
	cycle(5158276, nil, "fully signed and posted just now: the stale window starts here")
	cycle(5158288, nil, "12 blocks after posting: not stale yet")
	cycle(5158289, []string{orig}, "13 blocks after posting and unconfirmed: one re-drive")

	// The contract appended the replacement: orig and repl now form one spend group.
	stMu.Lock()
	state[contractinterface.MigrationSweepPrefix+repl] = sweepRecordHex(7)
	g := binary.BigEndian.AppendUint64(nil, 4209)
	g = binary.BigEndian.AppendUint16(g, 2)
	g = append(g, []byte(orig+repl)...)
	state[contractinterface.SpendGroupPrefix+"7"] = hex.EncodeToString(g)
	stMu.Unlock()
	gql.mu.Lock()
	gql.txSpends[repl] = &contractinterface.SigningData{}
	gql.mu.Unlock()
	db.txs[repl] = &database.Transaction{TxID: repl, State: database.TxStatePending, TotalSignatures: 17, CurrentSignatures: 0}

	cycle(5158290, []string{orig}, "the replaced original must never be re-driven again")
	cycle(5158310, []string{orig}, "the replacement is still collecting signatures")

	db.txs[repl].CurrentSignatures = 17
	db.txs[repl].State = database.TxStateSent
	cycle(5158311, []string{orig}, "the replacement was just posted")
	cycle(5158324, []string{orig, repl}, "the newest member, stale and unconfirmed, is the one re-driven")

	// The contract appended a third member; both older ones are now superseded.
	const repl2 = "9061bb4383f938737bd3529ea6b70f0ebdbfb87efc65951d046705ab5bd6cc4c"
	stMu.Lock()
	state[contractinterface.MigrationSweepPrefix+repl2] = sweepRecordHex(7)
	g = binary.BigEndian.AppendUint64(nil, 7015)
	g = binary.BigEndian.AppendUint16(g, 3)
	g = append(g, []byte(orig+repl+repl2)...)
	state[contractinterface.SpendGroupPrefix+"7"] = hex.EncodeToString(g)
	stMu.Unlock()
	gql.mu.Lock()
	gql.txSpends[repl2] = &contractinterface.SigningData{}
	gql.mu.Unlock()
	db.txs[repl2] = &database.Transaction{TxID: repl2, State: database.TxStatePending, TotalSignatures: 17, CurrentSignatures: 0}
	cycle(5158340, []string{orig, repl}, "neither replaced member is re-driven again")

	// repl2 is posted and mined, but its confirmSpend has not landed yet (testnet: the
	// bot's confirmSpend kept failing on RC): it stays in flight past the stale window.
	db.txs[repl2].CurrentSignatures = 17
	db.txs[repl2].State = database.TxStateSent
	cycle(5158341, []string{orig, repl}, "the newest member was just posted")
	btc.mu.Lock()
	btc.txStatuses[repl2] = true
	btc.mu.Unlock()
	cycle(5158360, []string{orig, repl}, "a sweep Bitcoin already confirmed is never re-driven")
}

func short(ids []string) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		if len(id) > 8 {
			id = id[:8]
		}
		out[i] = id
	}
	return out
}
