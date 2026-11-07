//go:build evm_devnet

// C1104 E2 rerun (11-05) for the two checks devnet E2r could not decide: both
// of its arms read the same for I-19 and for I-8, because the step did not
// produce the fault the bot met live. A GraphQL proxy in front of the bot's two
// endpoints now produces each fault the way the bot saw it on testnet:
//
//	I-19  the node refuses the bot's calls for RC ("not enough RCS available",
//	      the transaction pool's own text) right after a nonce move, for longer
//	      than B-R1b's 5-minute grace, then two calls go in one tick. develop's
//	      bot gives both the first call's nonce and the second is lost while
//	      logged as sent (testnet 11-04 07:18 nonce 76, 12:10 nonce 192); the
//	      fix gives each its own. E2r had only the quiet period, no refusal.
//	I-8   TX-GOSSIP-STALL: the node the bot submits to accepts the call and
//	      never gossips it while the bot's reads stay current (the proxy sends
//	      reads to a healthy node and submissions to a node cut off from its
//	      peers). develop's bot waits on the held call; the fix re-posts the
//	      same signed bytes to every endpoint after 2 min. E2r cut the bot's
//	      first endpoint off for reads too, so the bot read that node's frozen
//	      state and never sent the map (I-39): neither arm reached the path.
//
// Run per arm with that arm's artifacts (EVM_BOT_BIN etc.):
//
//	go test -tags evm_devnet -run TestEVMBridge_C1104_I19I8Rerun ./tests/devnet/ -timeout 160m -v
package devnet

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"vsc-node/lib/dids"

	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

// gqlFaultProxy forwards the bot's GraphQL requests to read; a
// submitTransactionV1 is refused with the transaction pool's RC refusal while
// refuse is set, or forwarded to submitTo when that is set.
type gqlFaultProxy struct {
	srv        *httptest.Server
	read       string
	submitTo   atomic.Pointer[string]
	refuse     atomic.Bool
	refused    atomic.Int64
	redirected atomic.Int64
}

func newGQLFaultProxy(t *testing.T, read, rcLimit string) *gqlFaultProxy {
	p := &gqlFaultProxy{read: read}
	client := &http.Client{Timeout: 30 * time.Second}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		target := p.read
		if bytes.Contains(body, []byte("submitTransactionV1")) {
			if p.refuse.Load() {
				p.refused.Add(1)
				w.Header().Set("Content-Type", "application/json")
				// transaction-pool.go:226, as the node's GraphQL returns it.
				fmt.Fprintf(w, `{"data":null,"errors":[{"message":"not enough RCS available: 4000 < %s","path":["submitTransactionV1"]}]}`, rcLimit)
				return
			}
			if s := p.submitTo.Load(); s != nil && *s != "" {
				target = *s
				p.redirected.Add(1)
			}
		}
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target, bytes.NewReader(body))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		req.Header.Set("Content-Type", r.Header.Get("Content-Type"))
		resp, err := client.Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func TestEVMBridge_C1104_I19I8Rerun(t *testing.T) {
	if testing.Short() {
		t.Skip("devnet")
	}
	ctx, e := a01Devnet(t, 150*time.Minute)
	anvil := StartAnvil(t, ctx, evmChainID, nextAnvilPort())
	t.Cleanup(anvil.Stop)
	eth := func(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(1e18)) }
	milli := func(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(1e15)) }

	type check struct {
		name, detail string
		fixed        bool
	}
	var checks []check
	record := func(name string, fixed bool, detail string) {
		checks = append(checks, check{name, detail, fixed})
		verdict := "BUG"
		if fixed {
			verdict = "FIXED"
		}
		t.Logf("CHECK %s: %s (%s)", name, verdict, detail)
	}
	t.Cleanup(func() {
		for _, c := range checks {
			verdict := "BUG"
			if c.fixed {
				verdict = "FIXED"
			}
			t.Logf("C1104 SUMMARY %-6s %-5s %s", c.name, verdict, c.detail)
			if !c.fixed {
				t.Errorf("%s: %s", c.name, c.detail)
			}
		}
	})

	// Mainnet weather on the bot's L1 reads, as in E2.
	rpcA, err := StartChaosRPC(anvil.URL, MainnetWeather(2204))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rpcA.Stop)
	rpcB, err := StartChaosRPC(anvil.URL, MainnetWeather(2205))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rpcB.Stop)

	botKey := mustKey(t)
	botDID := dids.NewEthDID(ethCrypto.PubkeyToAddress(botKey.PublicKey).Hex()).String()
	if err := e.proposeExecute("registerRelayer", fmt.Sprintf(`{"account":"%s"}`, botDID)); err != nil {
		t.Fatalf("registerRelayer: %v", err)
	}
	if err := e.fundDID(botDID, "50000.000"); err != nil {
		t.Fatalf("fund bot DID: %v", err)
	}
	e.botAttests = true
	vault0, err := e.tssVault(25 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := anvil.SetBalance(ctx, vault0, eth(1)); err != nil {
		t.Fatal(err)
	}
	// R serves the bot's reads (both proxies); X takes the I-8 submissions.
	R := e.gqlNode
	X := 0
	for n := 2; n <= e.d.cfg.Nodes; n++ {
		if n != R && n != e.ownerNode {
			X = n
			break
		}
	}
	if X == 0 {
		t.Fatal("no node free for X")
	}
	const botRcLimit = "5000000" // StartBot's RC_LIMIT default
	p1 := newGQLFaultProxy(t, e.d.GQLEndpoint(R), botRcLimit)
	p2 := newGQLFaultProxy(t, e.d.GQLEndpoint(R), botRcLimit)
	stateAt := func(k string) string {
		m, err := e.d.GetStateByKeys(ctx, R, e.amID, []string{k})
		if err != nil || m == nil || m[k] == nil {
			return ""
		}
		return fmt.Sprintf("%v", m[k])
	}
	bot := StartBot(t, ctx, BotConfig{
		EthRPC: rpcA.URL + "," + rpcB.URL, ContractID: e.amID,
		GraphQLURL: p1.srv.URL + "," + p2.srv.URL,
		EthPrivHex: hex.EncodeToString(ethCrypto.FromECDSA(botKey)),
		Checkpoint: filepath.Join(t.TempDir(), "c1104i19i8-checkpoint.json"),
	})
	t.Cleanup(func() { bot.Stop() })
	botLogs := func() string { return strings.Join(bot.Logs(), "\n") }
	if !bot.WaitForLog(ctx, "A18: vault is the TSS key's address", evmL2Wait+15*time.Minute) {
		t.Fatalf("the bot did not take the vault:\n%s", botLogs())
	}
	credited := func(user string) *big.Int {
		v, _ := new(big.Int).SetString(stateAt("a-"+user+"-eth"), 10)
		if v == nil {
			return big.NewInt(0)
		}
		return v
	}
	plantFresh := func(b uint64) error {
		_ = anvil.Mine(ctx, 70)
		return e.plantFinalized(anvil, b)
	}
	mapNonces := func(from int) (map[string]int, int) {
		nonces := map[string]int{}
		re := regexp.MustCompile(`action=map nonce=(\d+)`)
		for i, l := range bot.Logs() {
			if i >= from {
				if m := re.FindStringSubmatch(l); m != nil {
					nonces[m[1]]++
				}
			}
		}
		dup := 0
		for _, c := range nonces {
			if c > 1 {
				dup++
			}
		}
		return nonces, dup
	}

	// (0) Baseline: a deposit maps through the proxies (the nonce move).
	u0Key := mustKey(t)
	u0 := evmDID(ethCrypto.PubkeyToAddress(u0Key.PublicKey))
	if err := anvil.SetBalance(ctx, ethCrypto.PubkeyToAddress(u0Key.PublicKey), eth(5)); err != nil {
		t.Fatal(err)
	}
	_, b0, err := anvil.SendETH(ctx, u0Key, vault0, eth(2))
	if err != nil {
		t.Fatal(err)
	}
	if err := plantFresh(b0); err != nil {
		t.Fatal(err)
	}
	if err := pollUntil(ctx, 12*time.Minute, func() bool { return credited(u0).Sign() > 0 }); err != nil {
		t.Fatalf("baseline deposit not mapped through the proxies\nbot:\n%s", botLogs())
	}
	t.Logf("(0) baseline deposit mapped: %s wei", credited(u0))

	// (1) I-19. Right after the move, the node refuses the bot's calls for RC
	// (both endpoints, as one node's RC is the account's) while two deposits in
	// one block wait to be mapped; after more than the 5-minute grace the
	// refusals stop and the bot maps both in one tick.
	u1Key, u2Key := mustKey(t), mustKey(t)
	u1A, u2A := ethCrypto.PubkeyToAddress(u1Key.PublicKey), ethCrypto.PubkeyToAddress(u2Key.PublicKey)
	u1, u2 := evmDID(u1A), evmDID(u2A)
	if err := anvil.SetBalance(ctx, u1A, eth(1)); err != nil {
		t.Fatal(err)
	}
	if err := anvil.SetBalance(ctx, u2A, eth(1)); err != nil {
		t.Fatal(err)
	}
	p1.refuse.Store(true)
	p2.refuse.Store(true)
	if err := anvil.rpc.CallContext(ctx, nil, "evm_setIntervalMining", 0); err != nil {
		t.Fatalf("evm_setIntervalMining 0: %v", err)
	}
	h1, err1 := anvil.sendETHNoWait(ctx, u1Key, 0, vault0, milli(300))
	h2, err2 := anvil.sendETHNoWait(ctx, u2Key, 0, vault0, milli(200))
	_ = anvil.rpc.CallContext(ctx, nil, "evm_mine")
	if err := anvil.rpc.CallContext(ctx, nil, "evm_setIntervalMining", 1); err != nil {
		t.Fatalf("evm_setIntervalMining 1: %v", err)
	}
	if err1 != nil || err2 != nil {
		t.Fatalf("send: %v %v", err1, err2)
	}
	r1, _ := anvil.waitReceipt(ctx, h1, 30*time.Second)
	r2, _ := anvil.waitReceipt(ctx, h2, 30*time.Second)
	if r1 == nil || r2 == nil || r1.BlockNumber.Uint64() != r2.BlockNumber.Uint64() {
		t.Fatalf("the two deposits are not in one block: %v %v", r1, r2)
	}
	if err := plantFresh(r1.BlockNumber.Uint64()); err != nil {
		t.Fatal(err)
	}
	refusedAt := devnetNow()
	if err := pollUntil(ctx, 5*time.Minute, func() bool { return p1.refused.Load()+p2.refused.Load() > 0 }); err != nil {
		record("I-19", false, "VACUOUS: the bot made no call while the node refused (nothing was refused)")
	} else {
		devnetSleep(6*time.Minute - devnetSince(refusedAt))
		refused := p1.refused.Load() + p2.refused.Load()
		p1.refuse.Store(false)
		p2.refuse.Store(false)
		liftAt, mapFrom := devnetNow(), len(bot.Logs())
		both := pollUntil(ctx, 10*time.Minute, func() bool { return credited(u1).Sign() > 0 && credited(u2).Sign() > 0 }) == nil
		nonces, dup := mapNonces(mapFrom)
		record("I-19", both && dup == 0, fmt.Sprintf("after %d RC refusals over 6 min, both maps credited within 10 min=%v (%s), map nonces %v, used twice=%d",
			refused, both, devnetSince(liftAt).Round(time.Second), nonces, dup))
	}

	// (2) I-8. The bot keeps reading current state through R, but its first
	// endpoint now hands submissions to X, cut off from every other node: X
	// accepts the map and never gossips it.
	others := []int{}
	for n := 1; n <= e.d.cfg.Nodes; n++ {
		if n != X {
			others = append(others, n)
		}
	}
	for _, n := range others {
		if err := e.d.Partition(ctx, X, n); err != nil {
			t.Fatalf("partition magi-%d/magi-%d: %v", X, n, err)
		}
	}
	xURL := e.d.GQLEndpoint(X)
	p1.submitTo.Store(&xURL)
	u3Key := mustKey(t)
	u3A := ethCrypto.PubkeyToAddress(u3Key.PublicKey)
	u3 := evmDID(u3A)
	if err := anvil.SetBalance(ctx, u3A, eth(1)); err != nil {
		t.Fatal(err)
	}
	_, b3, err := anvil.SendETH(ctx, u3Key, vault0, milli(100))
	if err != nil {
		t.Fatal(err)
	}
	if err := plantFresh(b3); err != nil {
		t.Fatal(err)
	}
	if err := pollUntil(ctx, 6*time.Minute, func() bool { return p1.redirected.Load() > 0 }); err != nil {
		record("I-8", false, "VACUOUS: the bot never submitted to the cut-off node")
	} else {
		sentAt := devnetNow()
		landed := pollUntil(ctx, 10*time.Minute, func() bool { return credited(u3).Sign() > 0 }) == nil
		record("I-8", landed, fmt.Sprintf("map held by a node that never gossips (%d submission(s) to it) reached the chain through the other endpoint within 10 min=%v (%s)",
			p1.redirected.Load(), landed, devnetSince(sentAt).Round(time.Second)))
	}
	p1.submitTo.Store(nil)
	for _, n := range others {
		_ = e.d.Heal(ctx, X, n)
	}

	for _, l := range bot.Logs() {
		if strings.Contains(l, "panic") {
			t.Errorf("bot log: %s", l)
		}
	}
	t.Logf("RPC faults met: A %s, B %s; proxy p1 refused %d redirected %d, p2 refused %d", rpcA, rpcB, p1.refused.Load(), p1.redirected.Load(), p2.refused.Load())
}
