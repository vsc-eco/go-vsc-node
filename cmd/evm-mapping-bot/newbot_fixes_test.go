package main

import (
	"context"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/core/types"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

// NEW-BOT-1: a token registered on the bridge (tsyms, ts-{symbol}) that the bot
// does not watch is an alarm; a watched one is not.
func TestNEWBOT1_RegisteredTokenTheBotDoesNotWatchIsAnAlarm(t *testing.T) {
	for _, watch := range []bool{false, true} {
		_, l2, gql, rpc := healthWorld(t, 10)
		l2.set("vsc1bridge", "tsyms", "usdc")
		l2.set("vsc1bridge", "ts-usdc", "11155111-"+strings.TrimPrefix(healthUSDC, "0x"))
		var tokens map[string]string
		if watch {
			// what the C4 check reads for a watched token, so it raises nothing else
			l2.set("vsc1bridge", "chainid", "11155111")
			l2.set("vsc1bridge", "t-11155111-"+healthUSDC[2:], "usdc|6|10000000")
			l2.set("vsc1bridge", "s-usdc", "0|0|0|0")
			tokens = map[string]string{healthUSDC: healthUSDC}
		}
		h := newBotHealth(12 * time.Second)
		now := time.Now()
		h.recordTick(now, nil, emptyCP())
		h.check(context.Background(), now, gql, rpc, "vsc1bridge", []string{"vsc1bridge-primary"}, "did:bot", []string{healthVault}, tokens, 0, nil)
		r := h.report(now)
		if got := hasIssue(r, "NEW-BOT-1: token usdc"); got == watch {
			t.Fatalf("watched=%v: NEW-BOT-1 alarm %v (%+v)", watch, got, r)
		}
	}
}

// NEW-BOT-2: the broadcast hash is the tx's own, whatever the RPC answers.
func TestNEWBOT2_BroadcastHashIsComputedNotTrusted(t *testing.T) {
	key, _ := ethCrypto.GenerateKey()
	to := ethCrypto.PubkeyToAddress(key.PublicKey)
	tx, err := types.SignTx(types.NewTx(&types.DynamicFeeTx{ChainID: big.NewInt(1), GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(30e9), Gas: 21000, To: &to, Value: big.NewInt(1)}),
		types.LatestSignerForChainID(big.NewInt(1)), key)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := tx.MarshalBinary()
	for _, answer := range []string{`"0x` + strings.Repeat("ab", 32) + `"`, `12345`, `"` + tx.Hash().Hex() + `"`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":` + answer + `}`))
		}))
		got, err := newEthRPC(srv.URL).broadcastTx(strings.TrimPrefix(hexOf(raw), "0x"))
		srv.Close()
		if err != nil || !strings.EqualFold(got, tx.Hash().Hex()) {
			t.Fatalf("RPC answered %s: broadcastTx = %q, %v; want the tx's own hash %s", answer, got, err, tx.Hash().Hex())
		}
	}
}

func hexOf(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, 2+2*len(b))
	out = append(out, '0', 'x')
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&0x0f])
	}
	return string(out)
}

// NEW-BOT-4: with NEEDS_FILE unset a wanted proof still starts the A10 clock.
func TestNEWBOT4_WantedProofIsTrackedWithoutNeedsFile(t *testing.T) {
	l2 := newFakeL2()
	l2.set("vsc1bridge", "zkverifier", "vsc1verifier")
	gql := l2.gql(t)
	t.Setenv("NEEDS_FILE", "")
	cfg := EVMBotConfig{ContractID: "vsc1bridge"}
	cp := &Checkpoint{HeaderNeeds: map[uint64]int64{150: time.Now().Unix()}}
	writeNeedsFile(context.Background(), gql, cp, cfg, 140, false)
	if cp.WantProofSince == 0 {
		t.Fatal("a header needed above the tip did not start the A10 clock with NEEDS_FILE unset")
	}
	cp.HeaderNeeds = map[uint64]int64{}
	writeNeedsFile(context.Background(), gql, cp, cfg, 140, false)
	if cp.WantProofSince != 0 {
		t.Fatalf("the clock did not stop once nothing is wanted: %d", cp.WantProofSince)
	}
}
