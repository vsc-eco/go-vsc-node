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

// healthWorld is a bridge with 10 ETH credited, a 0.05 ETH gas reserve, a bot
// identity with 100k RC and a vault holding vaultETH.
func healthWorld(t *testing.T, vaultETH float64) (*fakeL1, *fakeL2, *vscGraphQL, *ethRPC) {
	t.Helper()
	l1 := newFakeL1()
	l1.balances = map[string]*big.Int{healthVault: eth(vaultETH)}
	l2 := newFakeL2()
	l2.rc = map[string]int64{"did:bot": 100_000}
	l2.set("vsc1bridge", "gr", eth(0.05).String())
	l2.set("vsc1bridge", "s-eth", eth(10).String()+"|"+eth(10).String()+"|0|7")
	return l1, l2, l2.gql(t), l1.rpc(t)
}

func checked(t *testing.T, gql *vscGraphQL, rpc *ethRPC, cp *Checkpoint) healthReport {
	t.Helper()
	h := newBotHealth(12 * time.Second)
	now := time.Now()
	h.recordTick(now, nil, cp)
	h.check(context.Background(), now, gql, rpc, "vsc1bridge", "did:bot", healthVault)
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
	l2.set("vsc1bridge", "gr", eth(0.009).String())
	if r := checked(t, gql, rpc, emptyCP()); !hasIssue(r, "B12: gas reserve") {
		t.Fatalf("low gas reserve: %+v", r)
	}

	_, _, gql, rpc = healthWorld(t, 9.5)
	if r := checked(t, gql, rpc, emptyCP()); !hasIssue(r, "C4: vault") || !hasIssue(r, "500000000000000000 short") {
		t.Fatalf("vault short of custody: %+v", r)
	}
	// The queued withdrawal has left the vault but confirmSpend has not run.
	_, l2, gql, rpc = healthWorld(t, 9.5)
	l2.set("vsc1bridge", "n", "3")
	l2.set("vsc1bridge", "d-3", `{"nonce":3,"amount":500000000000000000,"asset":"eth","unsigned_tx_hex":"02"}`)
	if r := checked(t, gql, rpc, emptyCP()); hasIssue(r, "C4") {
		t.Fatalf("an in-flight withdrawal counted as a shortfall: %+v", r)
	}
	// ...but not an ERC-20 one.
	l2.set("vsc1bridge", "d-3", `{"nonce":3,"amount":500000000000000000,"asset":"usdc","unsigned_tx_hex":"02"}`)
	if r := checked(t, gql, rpc, emptyCP()); !hasIssue(r, "C4") {
		t.Fatalf("an ERC-20 withdrawal hid an ETH shortfall: %+v", r)
	}

	// A read that fails is an issue, not a pass.
	_, _, gql, _ = healthWorld(t, 10)
	if r := checked(t, gql, newEthRPC("http://127.0.0.1:1"), emptyCP()); !hasIssue(r, "C4: cannot read") {
		t.Fatalf("unreadable vault balance: %+v", r)
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
