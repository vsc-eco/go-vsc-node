package main

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const healthVault = "0x00000000000000000000000000000000000000aa"

func eth(n float64) *big.Int {
	v, _ := new(big.Float).Mul(big.NewFloat(n), big.NewFloat(1e18)).Int(nil)
	return v
}

// healthWorld is a bridge with 10 ETH credited, a 0.2 ETH gas reserve, a bot
// identity with 100k RC and a vault holding vaultETH.
func healthWorld(t *testing.T, vaultETH float64) (*fakeL1, *fakeL2, *vscGraphQL, *ethRPC) {
	t.Helper()
	l1 := newFakeL1()
	l1.balances = map[string]*big.Int{healthVault: eth(vaultETH)}
	l2 := newFakeL2()
	l2.rc = map[string]int64{"did:bot": 100_000}
	l2.keys["vsc1bridge-primary"] = [2]string{"active", tssPubG}
	l2.expiry = map[string]uint64{"vsc1bridge-primary": 365}
	l2.epoch = 100
	l2.set("vsc1bridge", "gr", eth(0.2).String())
	l2.set("vsc1bridge", "s-eth", eth(10).String()+"|"+eth(10).String()+"|0|7")
	return l1, l2, l2.gql(t), l1.rpc(t)
}

func checked(t *testing.T, gql *vscGraphQL, rpc *ethRPC, cp *Checkpoint) healthReport {
	t.Helper()
	h := newBotHealth(12 * time.Second)
	now := time.Now()
	h.recordTick(now, nil, cp)
	h.check(context.Background(), now, gql, rpc, "vsc1bridge", "vsc1bridge-primary", "did:bot", healthVault, nil)
	return h.report(now)
}

func emptyCP() *Checkpoint {
	return &Checkpoint{SentWithdrawals: map[string]SentTx{}, PendingMaps: map[string]PendingMap{}}
}

func hasIssue(r healthReport, sub string) bool {
	for _, i := range r.Issues {
		if strings.Contains(i, sub) {
			return true
		}
	}
	return false
}

// B11 / B12 / C4: the alarms fire on low RC, a low gas reserve and a vault
// short of what the bridge credited, and stay quiet when all is well.
func TestB11_HealthAlarms(t *testing.T) {
	_, _, gql, rpc := healthWorld(t, 10)
	if r := checked(t, gql, rpc, emptyCP()); r.Status != "ok" || len(r.Issues) != 0 {
		t.Fatalf("healthy bridge: %+v", r)
	}

	_, l2, gql, rpc := healthWorld(t, 10)
	l2.rc["did:bot"] = 19_999
	if r := checked(t, gql, rpc, emptyCP()); r.Status != "unhealthy" || !hasIssue(r, "B11: bot RC 19999") {
		t.Fatalf("low RC: %+v", r)
	}
	_, l2, gql, rpc = healthWorld(t, 10)
	l2.rc["did:bot"] = 20_000 // at the threshold: fine
	if r := checked(t, gql, rpc, emptyCP()); hasIssue(r, "B11") {
		t.Fatalf("RC at the threshold raised an alarm: %+v", r)
	}
	_, l2, gql, rpc = healthWorld(t, 10)
	delete(l2.rc, "did:bot") // no RC record: unknown, not an alarm
	if r := checked(t, gql, rpc, emptyCP()); hasIssue(r, "B11") {
		t.Fatalf("no RC record raised an alarm: %+v", r)
	}

	_, l2, gql, rpc = healthWorld(t, 10)
	l2.set("vsc1bridge", "gr", eth(0.06).String()) // above the contract's 0.05 floor, below 0.1
	if r := checked(t, gql, rpc, emptyCP()); !hasIssue(r, "B12: gas reserve") {
		t.Fatalf("low gas reserve: %+v", r)
	}
	_, l2, gql, rpc = healthWorld(t, 10)
	l2.set("vsc1bridge", "gr", eth(0.1).String())
	if r := checked(t, gql, rpc, emptyCP()); hasIssue(r, "B12") {
		t.Fatalf("gas reserve at the threshold raised an alarm: %+v", r)
	}

	_, _, gql, rpc = healthWorld(t, 9.5)
	if r := checked(t, gql, rpc, emptyCP()); !hasIssue(r, "C4: vault") || !hasIssue(r, "500000000000000000 short") {
		t.Fatalf("vault short of custody: %+v", r)
	}
	// A queued 0.5 ETH withdrawal, as the bridge books it: custody drops to
	// 9.5 at unmap; the vault holds 10 until the transfer mines, then 9.5.
	// Neither is a shortfall.
	for _, vaultETH := range []float64{10, 9.5} {
		_, l2, gql, rpc = healthWorld(t, vaultETH)
		l2.set("vsc1bridge", "s-eth", eth(9.5).String()+"|"+eth(9.5).String()+"|0|7")
		l2.set("vsc1bridge", "n", "3")
		l2.set("vsc1bridge", "d-3", `{"nonce":3,"amount":500000000000000000,"asset":"eth","unsigned_tx_hex":"02"}`)
		if r := checked(t, gql, rpc, emptyCP()); hasIssue(r, "C4") {
			t.Fatalf("vault %v ETH during a queued withdrawal: %+v", vaultETH, r)
		}
	}
	// A real shortfall while that withdrawal is pending is reported: counting
	// the pending 0.5 ETH on the vault's side would hide it.
	_, l2, gql, rpc = healthWorld(t, 9)
	l2.set("vsc1bridge", "s-eth", eth(9.5).String()+"|"+eth(9.5).String()+"|0|7")
	l2.set("vsc1bridge", "n", "3")
	l2.set("vsc1bridge", "d-3", `{"nonce":3,"amount":500000000000000000,"asset":"eth","unsigned_tx_hex":"02"}`)
	if r := checked(t, gql, rpc, emptyCP()); !hasIssue(r, "500000000000000000 short") {
		t.Fatalf("a pending withdrawal hid a shortfall: %+v", r)
	}
	// A custody record that does not parse is an issue, not a pass.
	_, l2, gql, rpc = healthWorld(t, 10)
	l2.set("vsc1bridge", "s-eth", "garbage|0|0|0")
	if r := checked(t, gql, rpc, emptyCP()); !hasIssue(r, "C4: cannot parse") {
		t.Fatalf("unparseable custody: %+v", r)
	}

	// D7: the key's expiry. 28 epochs out is a warning, 29 is not; an expired
	// (deprecated) key is; no expiry (0) never is.
	for _, tc := range []struct {
		status        string
		expiry, epoch uint64
		want          string
	}{
		{"active", 365, 336, ""},
		{"active", 365, 337, "expires in 28 epochs (~168h)"},
		{"active", 365, 400, "expires in 0 epochs"},
		{"deprecated", 365, 400, "has expired"},
		{"active", 0, 9999, ""},
	} {
		_, l2, gql, rpc := healthWorld(t, 10)
		l2.keys["vsc1bridge-primary"] = [2]string{tc.status, tssPubG}
		l2.expiry["vsc1bridge-primary"], l2.epoch = tc.expiry, tc.epoch
		r := checked(t, gql, rpc, emptyCP())
		if got := hasIssue(r, "D7"); got != (tc.want != "") || (tc.want != "" && !hasIssue(r, tc.want)) {
			t.Errorf("key %s expiry %d at epoch %d: issues %v, want %q", tc.status, tc.expiry, tc.epoch, r.Issues, tc.want)
		}
	}
	_, l2, gql, rpc = healthWorld(t, 10)
	delete(l2.keys, "vsc1bridge-primary")
	if r := checked(t, gql, rpc, emptyCP()); !hasIssue(r, "D7: cannot read") {
		t.Fatalf("unreadable key: %+v", r)
	}

	// A read that fails is an issue, not a pass.
	_, _, gql, _ = healthWorld(t, 10)
	if r := checked(t, gql, newEthRPC("http://127.0.0.1:1"), emptyCP()); !hasIssue(r, "C4: cannot read") {
		t.Fatalf("unreadable vault balance: %+v", r)
	}
}

const healthUSDC = "0x1c7d4b196cb0c7b01d743fbc6116a902379c7238"

// C4 for a watched ERC-20: the vault's balanceOf against the custody of the
// symbol the bridge registered for that token.
func TestC4_TokenCustodyAlarm(t *testing.T) {
	world := func(vaultUSDC int64) (*fakeL1, *fakeL2, *vscGraphQL, *ethRPC) {
		l1, l2, gql, rpc := healthWorld(t, 10)
		l1.tokens = map[string]map[string]*big.Int{healthUSDC: {healthVault: big.NewInt(vaultUSDC)}}
		l2.set("vsc1bridge", "chainid", "11155111")
		l2.set("vsc1bridge", "t-11155111-"+healthUSDC[2:], "usdc|6|10000000")
		l2.set("vsc1bridge", "s-usdc", "30000000|30000000|0|0")
		return l1, l2, gql, rpc
	}
	tokens := map[string]string{healthUSDC: healthUSDC}
	check := func(gql *vscGraphQL, rpc *ethRPC) healthReport {
		h := newBotHealth(12 * time.Second)
		now := time.Now()
		h.recordTick(now, nil, emptyCP())
		h.check(context.Background(), now, gql, rpc, "vsc1bridge", "vsc1bridge-primary", "did:bot", healthVault, tokens)
		return h.report(now)
	}

	for _, held := range []int64{30_000_000, 31_000_000} { // equal; more (a deposit not yet mapped)
		_, _, gql, rpc := world(held)
		if r := check(gql, rpc); r.Status != "ok" {
			t.Fatalf("vault holds %d of 30000000 usdc: %+v", held, r)
		}
	}
	_, _, gql, rpc := world(29_999_999)
	if r := check(gql, rpc); !hasIssue(r, "C4: vault "+healthVault+" holds 29999999 usdc, 1 short of the 30000000 usdc") {
		t.Fatalf("vault 1 short: %+v", r)
	}
	// The symbol is the registry's: custody under another symbol is not read.
	_, l2, gql, rpc := world(0)
	l2.set("vsc1bridge", "t-11155111-"+healthUSDC[2:], "usdcx|6|10000000")
	if r := check(gql, rpc); hasIssue(r, "C4: vault") {
		t.Fatalf("read the custody of a symbol the token is not registered under: %+v", r)
	}
	// No chain id stored: the bridge's default chain 1 names the entry.
	_, l2, gql, rpc = world(0)
	l2.set("vsc1bridge", "chainid", "")
	l2.set("vsc1bridge", "t-1-"+healthUSDC[2:], "usdc|6|10000000")
	if r := check(gql, rpc); !hasIssue(r, "30000000 short") {
		t.Fatalf("chain id default: %+v", r)
	}

	// Each failure is an issue, never a pass.
	_, l2, gql, rpc = world(30_000_000)
	l2.set("vsc1bridge", "t-11155111-"+healthUSDC[2:], "")
	if r := check(gql, rpc); !hasIssue(r, "C4: token "+healthUSDC+" is not registered on the bridge") {
		t.Fatalf("unregistered token: %+v", r)
	}
	l1, _, gql, rpc := world(30_000_000)
	l1.tokens = nil // no contract at the address: eth_call returns "0x"
	if r := check(gql, rpc); !hasIssue(r, "C4: cannot read the vault's usdc balance") {
		t.Fatalf("unreadable balance: %+v", r)
	}
	_, l2, gql, rpc = world(30_000_000)
	l2.set("vsc1bridge", "s-usdc", "x|0|0|0")
	if r := check(gql, rpc); !hasIssue(r, "C4: cannot parse the bridge's usdc custody") {
		t.Fatalf("unparseable custody: %+v", r)
	}
}

// TOKENS adds watched ERC-20s on any network; mainnet always watches USDC; a
// malformed entry is refused, not skipped.
func TestTokensEnv(t *testing.T) {
	got, err := parseTokens("testnet", " 0x1C7D4B196Cb0C7B01d743Fbc6116a902379C7238 ,,0x00000000000000000000000000000000000000bb")
	if err != nil || len(got) != 2 || got[healthUSDC] == "" || got["0x00000000000000000000000000000000000000bb"] == "" {
		t.Fatalf("testnet TOKENS: %v %v", got, err)
	}
	if got, err := parseTokens("testnet", ""); err != nil || len(got) != 0 {
		t.Fatalf("testnet without TOKENS watches %v (%v), want none", got, err)
	}
	got, err = parseTokens("mainnet", healthUSDC)
	if err != nil || len(got) != 2 || got["0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48"] != "usdc" {
		t.Fatalf("mainnet: %v %v", got, err)
	}
	for _, bad := range []string{"0x1234", "1c7d4b196cb0c7b01d743fbc6116a902379c7238", "0x0000000000000000000000000000000000000000", healthUSDC + ",0xzz"} {
		if _, err := parseTokens("testnet", bad); err == nil {
			t.Errorf("TOKENS=%q accepted", bad)
		}
	}
}

// The report: starting before the first tick, stale ticks, a failed tick and
// dead letters are issues; GET /health answers 503 with them, 200 when ok.
func TestB11_HealthReport(t *testing.T) {
	h := newBotHealth(12 * time.Second)
	now := time.Now()
	if r := h.report(now); r.Status != "starting" {
		t.Fatalf("before a tick: %+v", r)
	}
	cp := emptyCP()
	cp.LastScannedBlock = 77
	h.recordTick(now, nil, cp)
	if r := h.report(now.Add(59 * time.Second)); r.Status != "ok" || r.LastScannedBlock != 77 {
		t.Fatalf("fresh tick: %+v", r)
	}
	if r := h.report(now.Add(61 * time.Second)); !hasIssue(r, "no tick for 61s") {
		t.Fatalf("stale: %+v", r)
	}
	cp.DeadLetters = []DeadLetter{{Block: 5, DepositTx: "0x1"}}
	h.recordTick(now, context.DeadlineExceeded, cp)
	r := h.report(now)
	if !hasIssue(r, "last tick failed") || !hasIssue(r, "1 deposit(s) dead-lettered") {
		t.Fatalf("failed tick + dead letter: %+v", r)
	}

	srv := httptest.NewServer(h.handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	var got healthReport
	_ = json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || got.Status != "unhealthy" || got.DeadLetters != 1 {
		t.Fatalf("GET /health: %d %+v", resp.StatusCode, got)
	}
	h.recordTick(time.Now(), nil, emptyCP())
	resp, _ = http.Get(srv.URL)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthy GET /health: %d", resp.StatusCode)
	}
}
