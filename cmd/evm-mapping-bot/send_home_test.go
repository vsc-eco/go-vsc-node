package main

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"vsc-node/lib/dids"

	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

// PAYOUT-1 keeper: the bridge's owed-home list ("ohl") is sent home with
// sendHome owed=1, a few a tick, after a dry run, while the bot keeps its RC
// reserve. A plain deposit is never on that list.

const (
	shA = "0x00000000000000000000000000000000000000A1"
	shB = "0x00000000000000000000000000000000000000b2"
)

func shWorld(t *testing.T) (*fakeL2, *vscGraphQL, *l2Submitter, EVMBotConfig) {
	t.Helper()
	lastSendHomeAt = time.Time{}
	sendHomeState = map[string]*sendHomeEntry{}
	l2 := newFakeL2()
	l2.rc = map[string]int64{}
	gql := l2.gql(t)
	key, _ := ethCrypto.GenerateKey()
	cfg := EVMBotConfig{ContractID: "vsc1bridge", NetID: "vsc-devnet", RcLimit: 10000}
	sub := &l2Submitter{ethKey: key, did: dids.NewEthDID(ethCrypto.PubkeyToAddress(key.PublicKey).Hex()), gql: gql, cfg: cfg}
	l2.rc[sub.did.String()] = 1_000_000
	l2.sim = func(string) map[string]any { return map[string]any{"success": true, "rc_used": 6000} }
	return l2, gql, sub, cfg
}

func shPayloads(t *testing.T, l2 *fakeL2) []string {
	t.Helper()
	var out []string
	for _, c := range submittedCalls(t, l2) {
		if c.Action == "sendHome" {
			// The bridge parses the raw input with url.ParseQuery: a JSON
			// string (quoted, & as &) fails on chain while a dry run
			// passes it (devnet 11-22: every owed refund failed).
			q, err := url.ParseQuery(c.Payload)
			if err != nil || !strings.HasPrefix(q.Get("address"), "0x") || len(q.Get("address")) != 42 {
				t.Fatalf("sendHome payload %q is not the plain query string the bridge parses", c.Payload)
			}
			out = append(out, c.Payload)
		}
	}
	return out
}

func TestSendHomeKeeper_SendsEachOwedEntry(t *testing.T) {
	l2, gql, sub, cfg := shWorld(t)
	l2.set("vsc1bridge", "ohl", "eth|did:pkh:eip155:1:"+shA+",usdc|did:pkh:eip155:1:"+shB+",eth|hive:alice")
	sendHomeOwed(context.Background(), gql, sub, cfg, nil)
	got := shPayloads(t, l2)
	want := []string{"address=" + shA + "&asset=eth&owed=1", "address=" + shB + "&asset=usdc&owed=1"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("sendHome calls %q, want %q (the hive: entry is not an Ethereum address)", got, want)
	}
	// Next tick, within sendHomeResend: the results are awaited, nothing resent.
	lastSendHomeAt = time.Time{}
	sendHomeOwed(context.Background(), gql, sub, cfg, nil)
	if n := len(shPayloads(t, l2)); n != 2 {
		t.Fatalf("%d sendHome calls after the second tick, want 2", n)
	}
	// Sent home: the entries leave the list and the keeper forgets them.
	l2.set("vsc1bridge", "ohl", "")
	lastSendHomeAt = time.Time{}
	sendHomeOwed(context.Background(), gql, sub, cfg, nil)
	if len(sendHomeState) != 0 {
		t.Fatalf("state %v after the list emptied", sendHomeState)
	}
}

func TestSendHomeKeeper_RefusedDryRunIsNotSentAndBacksOff(t *testing.T) {
	l2, gql, sub, cfg := shWorld(t)
	l2.set("vsc1bridge", "ohl", "eth|did:pkh:eip155:1:"+shA)
	l2.sim = func(string) map[string]any {
		return map[string]any{"success": false, "err": "intent_error", "err_msg": "the bridge cannot send this withdrawal now"}
	}
	sendHomeOwed(context.Background(), gql, sub, cfg, nil)
	if n := len(shPayloads(t, l2)); n != 0 {
		t.Fatalf("a refused dry run was broadcast (%d calls)", n)
	}
	e := sendHomeState["eth|did:pkh:eip155:1:"+shA]
	if e == nil || e.backoff != sendHomeEvery || e.refusedAt.IsZero() {
		t.Fatalf("entry %+v, want a first backoff of %s", e, sendHomeEvery)
	}
	// Retried after the backoff, doubled each refusal up to the cap.
	for i := 0; i < 10; i++ {
		e.nextTry = time.Time{}
		lastSendHomeAt = time.Time{}
		sendHomeOwed(context.Background(), gql, sub, cfg, nil)
	}
	if e.backoff != sendHomeBackoffMax {
		t.Fatalf("backoff %s, want the cap %s", e.backoff, sendHomeBackoffMax)
	}
	// It goes out once the bridge accepts it.
	l2.sim = func(string) map[string]any { return map[string]any{"success": true, "rc_used": 6000} }
	e.nextTry = time.Time{}
	lastSendHomeAt = time.Time{}
	sendHomeOwed(context.Background(), gql, sub, cfg, nil)
	if n := len(shPayloads(t, l2)); n != 1 || !e.refusedAt.IsZero() {
		t.Fatalf("%d calls, refusedAt %v: want sent and the streak reset", n, e.refusedAt)
	}
}

func TestSendHomeKeeper_WaitsBelowTheRCReserveAndCapsCallsPerTick(t *testing.T) {
	l2, gql, sub, cfg := shWorld(t)
	var list []string
	for _, c := range []string{"1", "2", "3", "4", "5"} {
		list = append(list, "eth|did:pkh:eip155:1:0x000000000000000000000000000000000000000"+c)
	}
	l2.set("vsc1bridge", "ohl", strings.Join(list, ","))
	l2.rc[sub.did.String()] = sendHomeRcReserve - 1
	sendHomeOwed(context.Background(), gql, sub, cfg, nil)
	if n := len(shPayloads(t, l2)); n != 0 {
		t.Fatalf("%d calls below the RC reserve, want none", n)
	}
	l2.rc[sub.did.String()] = 1_000_000
	lastSendHomeAt = time.Time{}
	sendHomeOwed(context.Background(), gql, sub, cfg, nil)
	if n := len(shPayloads(t, l2)); n != sendHomePerTick {
		t.Fatalf("%d calls in one tick, want %d", n, sendHomePerTick)
	}
	lastSendHomeAt = time.Time{}
	sendHomeOwed(context.Background(), gql, sub, cfg, nil)
	if n := len(shPayloads(t, l2)); n != 5 {
		t.Fatalf("%d calls after two ticks, want all 5", n)
	}
}

func TestSendHomeKeeper_NothingWhilePausedOrListEmpty(t *testing.T) {
	l2, gql, sub, cfg := shWorld(t)
	l2.set("vsc1bridge", "ohl", "eth|did:pkh:eip155:1:"+shA)
	cfg.Paused = true
	sendHomeOwed(context.Background(), gql, sub, cfg, nil)
	cfg.Paused = false
	l2.set("vsc1bridge", "ohl", "")
	lastSendHomeAt = time.Time{}
	sendHomeOwed(context.Background(), gql, sub, cfg, nil)
	if n := len(shPayloads(t, l2)); n != 0 {
		t.Fatalf("%d calls, want none", n)
	}
}

func TestOwedHomeCall(t *testing.T) {
	for in, want := range map[string]string{
		"eth|did:pkh:eip155:1:" + shA:  "address=" + shA + "&asset=eth&owed=1",
		"USDC|did:pkh:eip155:1:" + shA: "address=" + shA + "&asset=USDC&owed=1",
		"eth|hive:alice":               "",
		"eth|did:pkh:eip155:1:0x12":    "",
		"nopipe":                       "",
		"|did:pkh:eip155:1:" + shA:     "",
	} {
		if got := owedHomeCall(in); got != want {
			t.Errorf("owedHomeCall(%q) = %q, want %q", in, got, want)
		}
	}
}

// The swap back is sent at 99% of what the bot's dry run gave (today's
// price), never unpriced.
func TestHeldReturnMinOut(t *testing.T) {
	for in, want := range map[string]string{"1000": "990", `"1000"`: "990", "123": "121", "1": "1"} {
		if got, ok := heldReturnMinOut(in); !ok || got != want {
			t.Errorf("heldReturnMinOut(%s) = %q %v, want %s", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "0", "x", "-5"} {
		if _, ok := heldReturnMinOut(bad); ok {
			t.Errorf("heldReturnMinOut(%q) priced", bad)
		}
	}
}

// The keeper dry-runs each held return at min_out 1 and sends it at 99% of
// the output that dry run returned; a failed dry run sends nothing.
func TestHeldReturnKeeper_PricesFromItsDryRun(t *testing.T) {
	l2, gql, sub, cfg := shWorld(t)
	lastHeldReturnsAt, heldReturnState, heldReturnsCursor = time.Time{}, map[uint64]*sendHomeEntry{}, 0
	l2.set("vsc1bridge", "phrh", "0")
	l2.set("vsc1bridge", "phrt", "1")
	l2.set("vsc1bridge", "phr-0", `{"t":"hive:alice","a":"eth","d":"5","ra":"hbd","m":"7","h":1}`)
	l2.sim = func(string) map[string]any {
		return map[string]any{"success": false, "err": "x", "err_msg": "no pool", "rc_used": 3000}
	}
	retryHeldReturns(context.Background(), gql, sub, cfg)
	for _, c := range submittedCalls(t, l2) {
		if c.Action == "retryPayoutReturn" {
			t.Fatalf("sent after a failed dry run: %s", c.Payload)
		}
	}
	lastHeldReturnsAt, heldReturnState = time.Time{}, map[uint64]*sendHomeEntry{}
	l2.sim = func(string) map[string]any { return map[string]any{"success": true, "ret": "1000", "rc_used": 3000} }
	retryHeldReturns(context.Background(), gql, sub, cfg)
	var sent []string
	for _, c := range submittedCalls(t, l2) {
		if c.Action == "retryPayoutReturn" {
			sent = append(sent, c.Payload)
		}
	}
	if len(sent) != 1 || !strings.Contains(sent[0], `"min_out":"990"`) || !strings.Contains(sent[0], `"id":0`) {
		t.Fatalf("sent %v, want id 0 at min_out 990", sent)
	}
}

// The keeper's window moves every tick and wraps, so 32+ stuck returns at the
// head never hide the ones behind them.
func TestHeldReturnsWindowMoves(t *testing.T) {
	type w struct{ start, end, next uint64 }
	cases := []struct {
		head, tail, cursor uint64
		want               w
	}{
		{0, 10, 0, w{0, 10, 0}},      // all fit: wraps to the head
		{0, 100, 0, w{0, 32, 32}},    // first window
		{0, 100, 32, w{32, 64, 64}},  // moves on
		{0, 100, 96, w{96, 100, 0}},  // last part, wraps
		{40, 100, 10, w{40, 72, 72}}, // cursor behind the head: from the head
		{40, 100, 200, w{40, 72, 72}},
	}
	for _, c := range cases {
		s, e, n := heldReturnsWindow(c.head, c.tail, c.cursor)
		if (w{s, e, n}) != c.want {
			t.Errorf("head %d tail %d cursor %d: %d..%d next %d, want %+v", c.head, c.tail, c.cursor, s, e, n, c.want)
		}
	}
}

// An owed ETH send carries the gas_limit its address needs (estimate + 20%,
// within 21,000..300,000); a token send and a failed estimate carry none.
func TestSendHomeKeeper_OwedETHSendCarriesItsGasLimit(t *testing.T) {
	l2, gql, sub, cfg := shWorld(t)
	l2.set("vsc1bridge", "ohl", "eth|did:pkh:eip155:1:"+shA+",usdc|did:pkh:eip155:1:"+shB)
	est := func(_ context.Context, to string) (uint64, error) {
		if !strings.EqualFold(to, shA) {
			t.Errorf("estimated for %s", to)
		}
		return 30_000, nil
	}
	sendHomeOwed(context.Background(), gql, sub, cfg, est)
	got := shPayloads(t, l2)
	want := []string{"address=" + shA + "&asset=eth&owed=1&gas_limit=36000", "address=" + shB + "&asset=usdc&owed=1"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("payloads %v, want %v", got, want)
	}
	for in, want := range map[uint64]uint64{1: 21_000, 21_000: 25_200, 250_001: 300_000, 1_000_000: 300_000} {
		if got := owedGasLimit(in); got != want {
			t.Errorf("owedGasLimit(%d) = %d, want %d", in, got, want)
		}
	}
}
