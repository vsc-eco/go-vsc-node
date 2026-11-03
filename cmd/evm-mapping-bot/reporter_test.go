package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vsc-node/lib/btcvault"
)

// fakeReporter answers a round: per action, what the dry run says; and
// records what is submitted.
type fakeReporter struct {
	gens      []vaultGen
	theft     map[uint32]bool // reportUnauthorizedSpend accepted for gen
	custody   string          // last proveCustody result ("custody held"/"custody short"/"" refused)
	flagTheft bool
	flagShort bool
	proofErr  error
	submitted []l2Call

	tokens     []tokenCustody
	tokenVerd  string          // last proveTokenCustody result
	tokenShort map[string]bool // per-token flags
	storageErr error
	storageReq []string // keys asked
}

func (f *fakeReporter) deps() reporterDeps {
	return reporterDeps{
		gens:   func() ([]vaultGen, error) { return f.gens, nil },
		header: func() (uint64, error) { return 500, nil },
		proof: func(addr string, h uint64) (accountProof, error) {
			if f.proofErr != nil {
				return accountProof{}, f.proofErr
			}
			return accountProof{BlockHeight: h, Proof: addr}, nil
		},
		tokens: func() ([]tokenCustody, error) { return f.tokens, nil },
		flags: func(symbols []string) (reporterFlags, error) {
			fl := reporterFlags{theft: f.flagTheft, short: map[string]bool{"eth": f.flagShort}}
			for _, s := range symbols {
				fl.short[s] = f.tokenShort[s]
			}
			return fl, nil
		},
		storage: func(token string, keys []string, h uint64) (accountProof, []string, error) {
			f.storageReq = append(f.storageReq, keys...)
			if f.storageErr != nil {
				return accountProof{}, nil, f.storageErr
			}
			sps := make([]string, len(keys))
			for i, k := range keys {
				sps[i] = "sp" + k
			}
			return accountProof{BlockHeight: h, Proof: "acct" + token}, sps, nil
		},
		simulate: func(calls []l2Call) ([]simulatedCall, error) {
			out := make([]simulatedCall, len(calls))
			for i, c := range calls {
				var p struct {
					Gen uint32 `json:"gen"`
				}
				_ = json.Unmarshal(c.Payload, &p)
				switch c.Action {
				case "reportUnauthorizedSpend":
					out[i] = simulatedCall{Success: f.theft[p.Gen], RcUsed: 620, ErrMsg: "no unauthorized spend"}
				case "proveTokenRoot":
					out[i] = simulatedCall{Success: true, RcUsed: 700, Ret: "usdc storage root at block 500"}
				case "proveTokenCustody":
					out[i] = simulatedCall{Success: true, RcUsed: 650, Ret: "usdc custody proof at block 500: 1 of 2 generations"}
					if i == len(calls)-1 {
						out[i] = simulatedCall{Success: f.tokenVerd != "", RcUsed: 650, Ret: f.tokenVerd + " at block 500 (usdc)", ErrMsg: "no usdc storage root"}
					}
				case "proveCustody":
					out[i] = simulatedCall{Success: true, RcUsed: 532, Ret: "custody proof at block 500: 1 of 2 generations"}
					if i == len(calls)-1 {
						out[i] = simulatedCall{Success: f.custody != "", RcUsed: 532, Ret: f.custody + " at block 500", ErrMsg: "being assembled"}
					}
				}
			}
			return out, nil
		},
		submit: func(c l2Call, _ int64) error { f.submitted = append(f.submitted, c); return nil },
	}
}

func twoGens() []vaultGen {
	return []vaultGen{{Gen: 1, Status: btcvault.VaultStatusActive, Address: "0xa1"}, {Gen: 0, Status: btcvault.VaultStatusRetiring, Address: "0xb0"}}
}

// B6: a healthy bridge costs nothing; a theft or a custody change is sent.
func TestReporter_SendsOnlyWhatChangesSomething(t *testing.T) {
	f := &fakeReporter{gens: twoGens(), custody: "custody held"}
	if out, err := runReporter(f.deps()); err != nil || len(f.submitted) != 0 || !strings.HasPrefix(out, "nothing to report") {
		t.Fatalf("healthy: %q %v %v", out, err, f.submitted)
	}

	f = &fakeReporter{gens: twoGens(), custody: "custody held", theft: map[uint32]bool{0: true}}
	if out, err := runReporter(f.deps()); err != nil || len(f.submitted) != 1 || f.submitted[0].Action != "reportUnauthorizedSpend" ||
		!strings.Contains(string(f.submitted[0].Payload), `"gen":0`) || !strings.HasPrefix(out, "theft reported") {
		t.Fatalf("theft at gen 0: %q %v %v", out, err, f.submitted)
	}
	f = &fakeReporter{gens: twoGens(), custody: "custody held", theft: map[uint32]bool{0: true}, flagTheft: true}
	if _, err := runReporter(f.deps()); err != nil || len(f.submitted) != 0 {
		t.Fatalf("theft already halted: reported again %v", f.submitted)
	}

	f = &fakeReporter{gens: twoGens(), custody: "custody short"}
	out, err := runReporter(f.deps())
	if err != nil || len(f.submitted) != 2 || !strings.HasPrefix(out, "custody short reported") {
		t.Fatalf("short: %q %v %v", out, err, f.submitted)
	}
	for i, want := range []string{`"gen":1`, `"gen":0`} { // every generation, in view order
		if f.submitted[i].Action != "proveCustody" || !strings.Contains(string(f.submitted[i].Payload), want) {
			t.Fatalf("submission %d %s %s", i, f.submitted[i].Action, f.submitted[i].Payload)
		}
	}
	f = &fakeReporter{gens: twoGens(), custody: "custody short", flagShort: true}
	if _, err := runReporter(f.deps()); err != nil || len(f.submitted) != 0 {
		t.Fatalf("short and already halted: sent %v", f.submitted)
	}
	f = &fakeReporter{gens: twoGens(), custody: "custody held", flagShort: true}
	if out, err := runReporter(f.deps()); err != nil || len(f.submitted) != 2 || !strings.HasPrefix(out, "custody restored reported") {
		t.Fatalf("held while halted (lift): %q %v %v", out, err, f.submitted)
	}
}

// A round that cannot prove (pruned RPC) or whose proof the bridge would not
// take now sends nothing.
func TestReporter_SkipsWhatItCannotProve(t *testing.T) {
	f := &fakeReporter{gens: twoGens(), custody: "custody short", proofErr: errors.New("missing trie node")}
	if _, err := runReporter(f.deps()); err == nil || len(f.submitted) != 0 {
		t.Fatalf("eth_getProof failed: err %v sent %v", err, f.submitted)
	}
	f = &fakeReporter{gens: twoGens(), custody: ""} // refused (another proof being assembled)
	if out, err := runReporter(f.deps()); err != nil || len(f.submitted) != 0 || !strings.Contains(out, "not accepted now") {
		t.Fatalf("refused dry run: %q %v %v", out, err, f.submitted)
	}
	f = &fakeReporter{}
	if out, _ := runReporter(f.deps()); out != "no fund-holding generation" {
		t.Fatalf("no generations: %q", out)
	}
}

// A pre-rotation bridge's one vault is generation 0; a rotated bridge lists
// its fund-holding generations with an address.
func TestReporter_FundHoldingGens(t *testing.T) {
	if g := fundHoldingGens(vaultView{}, EVMBotConfig{VaultAddress: "0xABC"}); len(g) != 1 || g[0].Gen != 0 || g[0].Address != "0xabc" {
		t.Fatalf("pre-rotation: %+v", g)
	}
	view := vaultView{Registry: true, Gens: []vaultGen{
		{Gen: 0, Status: btcvault.VaultStatusPurged, Address: "0x0"},
		{Gen: 1, Status: btcvault.VaultStatusInactive, Address: "0x1"},
		{Gen: 2, Status: btcvault.VaultStatusActive, Address: "0x2"},
		{Gen: 3, Status: btcvault.VaultStatusPending},
	}}
	g := fundHoldingGens(view, EVMBotConfig{})
	if len(g) != 2 || g[0].Gen != 1 || g[1].Gen != 2 {
		t.Fatalf("rotated: %+v", g)
	}
}

func TestReporter_Interval(t *testing.T) {
	old, oldNow, oldLast := reporterEvery, reporterNow, lastReportAt
	t.Cleanup(func() { reporterEvery, reporterNow, lastReportAt = old, oldNow, oldLast })
	now := time.Unix(1_000_000, 0)
	reporterNow = func() time.Time { return now }
	reporterEvery, lastReportAt = 10*time.Minute, time.Time{}
	if !reporterDue() || reporterDue() {
		t.Fatal("first round must run, an immediate second must not")
	}
	now = now.Add(10 * time.Minute)
	if !reporterDue() {
		t.Fatal("a round after the interval did not run")
	}
	reporterEvery = 0
	now = now.Add(time.Hour)
	if reporterDue() {
		t.Fatal("REPORTER_EVERY=0 must turn the reporter off")
	}
}

func usdcCustody() []tokenCustody {
	return []tokenCustody{{Symbol: "usdc", Address: "0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48", Slot: 9}}
}

// Token custody: each token with a setting is proven at the same header as
// ETH, and sent only when its verdict differs from its own flag; a token's
// failure does not stop the ETH round.
func TestReporter_TokenRounds(t *testing.T) {
	f := &fakeReporter{gens: twoGens(), custody: "custody held", tokens: usdcCustody(), tokenVerd: "custody held"}
	out, err := runReporter(f.deps())
	if err != nil || len(f.submitted) != 0 || !strings.HasPrefix(out, "nothing to report") || !strings.Contains(out, "; usdc nothing to report") {
		t.Fatalf("healthy: %q %v %v", out, err, f.submitted)
	}
	if len(f.storageReq) != 2 || f.storageReq[0] != tokenSlotKey("0xa1", 9) || f.storageReq[1] != tokenSlotKey("0xb0", 9) {
		t.Fatalf("storage keys asked %v", f.storageReq)
	}

	f = &fakeReporter{gens: twoGens(), custody: "custody held", tokens: usdcCustody(), tokenVerd: "custody short"}
	out, err = runReporter(f.deps())
	if err != nil || len(f.submitted) != 3 || !strings.Contains(out, "usdc custody short reported") {
		t.Fatalf("usdc short: %q %v %v", out, err, f.submitted)
	}
	if f.submitted[0].Action != "proveTokenRoot" || !strings.Contains(string(f.submitted[0].Payload), `"asset":"usdc"`) ||
		!strings.Contains(string(f.submitted[0].Payload), `"block_height":500`) {
		t.Fatalf("root first: %s %s", f.submitted[0].Action, f.submitted[0].Payload)
	}
	for i, g := range []string{"0xa1", "0xb0"} { // every generation, in view order, its own key's proof
		var p struct {
			Asset string `json:"asset"`
			Gen   uint32 `json:"gen"`
			H     uint64 `json:"block_height"`
			SP    string `json:"storage_proof"`
		}
		c := f.submitted[i+1]
		if err := json.Unmarshal(c.Payload, &p); err != nil || c.Action != "proveTokenCustody" || p.Asset != "usdc" || p.H != 500 ||
			p.Gen != twoGens()[i].Gen || p.SP != "sp"+tokenSlotKey(g, 9) {
			t.Fatalf("submission %d: %s %s", i+1, c.Action, c.Payload)
		}
	}

	f = &fakeReporter{gens: twoGens(), custody: "custody held", tokens: usdcCustody(), tokenVerd: "custody short", tokenShort: map[string]bool{"usdc": true}}
	if out, err := runReporter(f.deps()); err != nil || len(f.submitted) != 0 {
		t.Fatalf("usdc short and already flagged: %q sent %v", out, f.submitted)
	}
	f = &fakeReporter{gens: twoGens(), custody: "custody held", tokens: usdcCustody(), tokenVerd: "custody held", tokenShort: map[string]bool{"usdc": true}}
	if out, err := runReporter(f.deps()); err != nil || len(f.submitted) != 3 || !strings.Contains(out, "usdc custody restored reported") {
		t.Fatalf("usdc held while flagged (lift): %q %v %v", out, err, f.submitted)
	}
	// the ETH flag is ETH's alone: USDC flagged short does not make ETH's
	// held verdict a change worth sending
	if len(f.submitted) != 3 || f.submitted[0].Action != "proveTokenRoot" {
		t.Fatalf("ETH proofs sent on USDC's flag: %v", f.submitted)
	}

	f = &fakeReporter{gens: twoGens(), custody: "custody short", tokens: usdcCustody(), tokenVerd: "custody short", storageErr: errors.New("missing trie node")}
	out, err = runReporter(f.deps())
	if err != nil || len(f.submitted) != 2 || !strings.HasPrefix(out, "custody short reported") || !strings.Contains(out, "usdc custody round skipped") {
		t.Fatalf("usdc storage proof failed, ETH short: %q %v %v", out, err, f.submitted)
	}
	f = &fakeReporter{gens: twoGens(), custody: "custody held", tokens: usdcCustody(), tokenVerd: ""}
	if out, err := runReporter(f.deps()); err != nil || len(f.submitted) != 0 || !strings.Contains(out, "usdc custody proof not accepted now") {
		t.Fatalf("usdc refused dry run: %q %v %v", out, err, f.submitted)
	}
}

func TestReporter_ParseFlagsAndTokens(t *testing.T) {
	if k := strings.Join(flagKeys([]string{"usdc"}), ","); k != "th,si,sis-eth,sis-usdc" {
		t.Fatalf("flag keys read: %s", k)
	}
	fl := parseReporterFlags(map[string]string{"si": "900"}, []string{"usdc"})
	if !fl.short["eth"] || fl.short["usdc"] {
		t.Fatalf("legacy si (no asset flag) is ETH's: %+v", fl)
	}
	fl = parseReporterFlags(map[string]string{"si": "900", "sis-usdc": "900"}, []string{"usdc"})
	if fl.short["eth"] || !fl.short["usdc"] {
		t.Fatalf("usdc's halt: %+v", fl)
	}
	fl = parseReporterFlags(map[string]string{"th": "1", "si": "9", "sis-eth": "9"}, nil)
	if !fl.theft || !fl.short["eth"] {
		t.Fatalf("theft and eth: %+v", fl)
	}
	toks := parseTokenCustody(map[string]string{
		"tcc-usdc": "9|255|1000000", "ts-usdc": "1-A0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48",
		"ts-dai": "1-6b175474e89094c44da98b954eedeac495271d0f", // no setting
		"tcc-bad": "x|255|0", "ts-bad": "1-6b175474e89094c44da98b954eedeac495271d0f",
	}, []string{"usdc", "dai", "bad"})
	if len(toks) != 1 || toks[0] != (tokenCustody{Symbol: "usdc", Address: "0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48", Slot: 9}) {
		t.Fatalf("tokens %+v", toks)
	}
}

// The slot key of a mainnet USDC holder at slot 9, as cast index computes it.
func TestReporter_TokenSlotKey(t *testing.T) {
	if got := tokenSlotKey("0x28C6c06298d514Db089934071355E5743bf21d60", 9); got != "0x07081a045c3dbf2e63b62a407ef205e7586e2629d2e2b95ff093308ca0ff3727" {
		t.Fatalf("slot key %s", got)
	}
}

// fetchStorageProofs keeps the asked key order and refuses an answer whose
// proofs are for other keys or are missing; an empty storage trie (no nodes)
// gives "" for each key.
func TestFetchStorageProofs(t *testing.T) {
	var answer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(answer))
	}))
	defer srv.Close()
	rpc := newEthRPC(srv.URL)
	k1, k2 := tokenSlotKey("0xa1", 9), tokenSlotKey("0xb0", 9)
	res := func(sp string) string {
		return `{"jsonrpc":"2.0","id":1,"result":{"accountProof":["0x0102","0x03"],"storageProof":[` + sp + `]}}`
	}
	answer = res(`{"key":"` + k1 + `","value":"0x5","proof":["0xaa","0xbb"]},{"key":"` + k2 + `","value":"0x0","proof":[]}`)
	acct, sps, err := fetchStorageProofs(context.Background(), rpc, "0xtoken", []string{k1, k2}, 77)
	if err != nil || acct.BlockHeight != 77 || acct.Proof != base64.StdEncoding.EncodeToString([]byte{1, 2, 3}) ||
		len(sps) != 2 || sps[0] != base64.StdEncoding.EncodeToString([]byte{0xaa, 0xbb}) || sps[1] != "" {
		t.Fatalf("%+v %v %v", acct, sps, err)
	}
	answer = res(`{"key":"` + k2 + `","proof":["0xaa"]},{"key":"` + k1 + `","proof":["0xbb"]}`)
	if _, _, err := fetchStorageProofs(context.Background(), rpc, "0xtoken", []string{k1, k2}, 77); err == nil {
		t.Fatal("proofs in another key order were taken")
	}
	answer = res(`{"key":"` + k1 + `","proof":["0xaa"]}`)
	if _, _, err := fetchStorageProofs(context.Background(), rpc, "0xtoken", []string{k1, k2}, 77); err == nil {
		t.Fatal("a missing proof was taken")
	}
	// a key echoed without leading zeros is the same key
	short := "0x" + strings.TrimLeft(strings.TrimPrefix(k1, "0x"), "0")
	answer = res(`{"key":"` + short + `","proof":["0xaa"]}`)
	if _, _, err := fetchStorageProofs(context.Background(), rpc, "0xtoken", []string{k1}, 77); err != nil {
		t.Fatalf("a zero-trimmed echo of the key: %v", err)
	}
}
